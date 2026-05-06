package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/util/responseheaders"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"
)

// ForwardAsChatCompletions accepts a Chat Completions request body and forwards
// it to the best upstream path for the selected account:
// - Copilot/Kiro: native provider-specific forwarding
// - API key accounts: native OpenAI-compatible /chat/completions
// - OAuth ChatGPT accounts: convert to Responses API and convert back
func (s *OpenAIGatewayService) ForwardAsChatCompletions(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	promptCacheKey string,
	defaultMappedModel string,
) (*OpenAIForwardResult, error) {
	startTime := time.Now()

	// 1. Parse Chat Completions request
	var chatReq apicompat.ChatCompletionsRequest
	if err := json.Unmarshal(body, &chatReq); err != nil {
		return nil, fmt.Errorf("parse chat completions request: %w", err)
	}
	originalModel := chatReq.Model
	clientStream := chatReq.Stream
	includeUsage := chatReq.StreamOptions != nil && chatReq.StreamOptions.IncludeUsage

	// 2. Resolve model mapping early so compat prompt_cache_key injection can
	// derive a stable seed from the final upstream model family.
	billingModel, _ := s.ResolveUpstreamModelForAccount(ctx, account, originalModel, defaultMappedModel)
	upstreamModel := normalizeOpenAIModelForUpstream(account, billingModel)

	promptCacheKey = strings.TrimSpace(promptCacheKey)
	compatPromptCacheInjected := false
	if promptCacheKey == "" && account.Type == AccountTypeOAuth && shouldAutoInjectPromptCacheKeyForCompat(upstreamModel) {
		promptCacheKey = deriveCompatPromptCacheKey(&chatReq, upstreamModel)
		compatPromptCacheInjected = promptCacheKey != ""
	}

	// 3a. Copilot: forward as native chat completions (Copilot doesn't support Responses API).
	if account.IsCopilot() {
		return s.forwardCopilotChatCompletions(ctx, c, account, body, originalModel, billingModel, upstreamModel, clientStream, includeUsage, startTime)
	}

	// 3b. Kiro: forward via Kiro generateAssistantResponse API.
	if account.IsKiro() {
		return s.forwardKiroChatCompletions(ctx, c, account, body, originalModel, billingModel, upstreamModel, clientStream, includeUsage, startTime)
	}

	// 3c. API key accounts should prefer the native OpenAI-compatible
	// /chat/completions endpoint. Many proxy providers support chat completions
	// but do not implement /v1/responses.
	if account.Type == AccountTypeAPIKey {
		return s.forwardNativeAPIKeyChatCompletions(ctx, c, account, body, originalModel, billingModel, upstreamModel, clientStream, startTime)
	}

	// 3. Convert to Responses and forward
	// ChatCompletionsToResponses always sets Stream=true (upstream always streams).
	responsesReq, err := apicompat.ChatCompletionsToResponses(&chatReq)
	if err != nil {
		return nil, fmt.Errorf("convert chat completions to responses: %w", err)
	}
	responsesReq.Model = upstreamModel

	logFields := []zap.Field{
		zap.Int64("account_id", account.ID),
		zap.String("original_model", originalModel),
		zap.String("billing_model", billingModel),
		zap.String("upstream_model", upstreamModel),
		zap.Bool("stream", clientStream),
	}
	if compatPromptCacheInjected {
		logFields = append(logFields,
			zap.Bool("compat_prompt_cache_key_injected", true),
			zap.String("compat_prompt_cache_key_sha256", hashSensitiveValueForLog(promptCacheKey)),
		)
	}
	logger.L().Debug("openai chat_completions: model mapping applied", logFields...)

	// 4. Marshal Responses request body, then apply OAuth codex transform
	responsesBody, err := json.Marshal(responsesReq)
	if err != nil {
		return nil, fmt.Errorf("marshal responses request: %w", err)
	}

	// Copilot uses the standard Responses API format; codex transform is ChatGPT-internal only.
	if account.Type == AccountTypeOAuth && !account.IsCopilot() && !account.IsKiro() {
		var reqBody map[string]any
		if err := json.Unmarshal(responsesBody, &reqBody); err != nil {
			return nil, fmt.Errorf("unmarshal for codex transform: %w", err)
		}
		codexResult := applyCodexOAuthTransform(reqBody, false, false)
		if codexResult.NormalizedModel != "" {
			upstreamModel = codexResult.NormalizedModel
		}
		if codexResult.PromptCacheKey != "" {
			promptCacheKey = codexResult.PromptCacheKey
		} else if promptCacheKey != "" {
			reqBody["prompt_cache_key"] = promptCacheKey
		}
		responsesBody, err = json.Marshal(reqBody)
		if err != nil {
			return nil, fmt.Errorf("remarshal after codex transform: %w", err)
		}
	}

	// 5. Get access token
	token, _, err := s.GetAccessToken(ctx, account)
	if err != nil {
		return nil, fmt.Errorf("get access token: %w", err)
	}

	// 6. Build upstream request
	upstreamReq, err := s.buildUpstreamRequest(ctx, c, account, responsesBody, token, true, promptCacheKey, false)
	if err != nil {
		return nil, fmt.Errorf("build upstream request: %w", err)
	}

	if promptCacheKey != "" {
		upstreamReq.Header.Set("session_id", generateSessionUUID(promptCacheKey))
	}

	// 7. Send request
	proxyURL := ""
	if account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	resp, err := s.httpUpstream.Do(upstreamReq, proxyURL, account.ID, account.Concurrency)
	if err != nil {
		safeErr := sanitizeUpstreamErrorMessage(err.Error())
		setOpsUpstreamError(c, 0, safeErr, "")
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			Platform:           account.Platform,
			AccountID:          account.ID,
			AccountName:        account.Name,
			UpstreamStatusCode: 0,
			Kind:               "request_error",
			Message:            safeErr,
		})
		writeChatCompletionsError(c, http.StatusBadGateway, "upstream_error", "Upstream request failed")
		return nil, fmt.Errorf("upstream request failed: %s", safeErr)
	}
	defer func() { _ = resp.Body.Close() }()

	// 8. Handle error response with failover
	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		_ = resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(respBody))

		upstreamMsg := strings.TrimSpace(extractUpstreamErrorMessage(respBody))
		upstreamMsg = sanitizeUpstreamErrorMessage(upstreamMsg)
		if s.shouldFailoverOpenAIUpstreamResponse(resp.StatusCode, upstreamMsg, respBody) {
			upstreamDetail := ""
			if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
				maxBytes := s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes
				if maxBytes <= 0 {
					maxBytes = 2048
				}
				upstreamDetail = truncateString(string(respBody), maxBytes)
			}
			appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
				Platform:           account.Platform,
				AccountID:          account.ID,
				AccountName:        account.Name,
				UpstreamStatusCode: resp.StatusCode,
				UpstreamRequestID:  resp.Header.Get("x-request-id"),
				Kind:               "failover",
				Message:            upstreamMsg,
				Detail:             upstreamDetail,
			})
			if s.rateLimitService != nil {
				s.rateLimitService.HandleUpstreamError(ctx, account, resp.StatusCode, resp.Header, respBody)
			}
			return nil, buildOpenAIUpstreamFailoverError(account, resp.StatusCode, upstreamMsg, respBody)
		}
		if shouldBridgeOpenAICompatibilityError(account, resp.StatusCode, upstreamMsg) {
			return nil, &OpenAIBridgeFallbackError{
				BridgePlatform: PlatformOpenAI,
				Endpoint:       openAIBridgeEndpointChat,
				StatusCode:     resp.StatusCode,
				Message:        upstreamMsg,
			}
		}
		return s.handleChatCompletionsErrorResponse(resp, c, account)
	}

	// 9. Handle normal response
	var result *OpenAIForwardResult
	var handleErr error
	if clientStream {
		result, handleErr = s.handleChatStreamingResponse(resp, c, originalModel, billingModel, upstreamModel, includeUsage, startTime)
	} else {
		result, handleErr = s.handleChatBufferedStreamingResponse(resp, c, originalModel, billingModel, upstreamModel, startTime)
	}

	// Propagate ServiceTier and ReasoningEffort to result for billing
	if handleErr == nil && result != nil {
		if responsesReq.ServiceTier != "" {
			st := responsesReq.ServiceTier
			result.ServiceTier = &st
		}
		if responsesReq.Reasoning != nil && responsesReq.Reasoning.Effort != "" {
			re := responsesReq.Reasoning.Effort
			result.ReasoningEffort = &re
		}
	}

	// Extract and save Codex usage snapshot from response headers (for OAuth accounts)
	if handleErr == nil && account.Type == AccountTypeOAuth {
		if snapshot := ParseCodexRateLimitHeaders(resp.Header); snapshot != nil {
			s.updateCodexUsageSnapshot(ctx, account.ID, snapshot)
		}
	}

	return result, handleErr
}

// handleChatCompletionsErrorResponse reads an upstream error and returns it in
// OpenAI Chat Completions error format.
func (s *OpenAIGatewayService) handleChatCompletionsErrorResponse(
	resp *http.Response,
	c *gin.Context,
	account *Account,
) (*OpenAIForwardResult, error) {
	return s.handleCompatErrorResponse(resp, c, account, writeChatCompletionsError)
}

// handleChatBufferedStreamingResponse reads all Responses SSE events from the
// upstream, finds the terminal event, converts to a Chat Completions JSON
// response, and writes it to the client.
func (s *OpenAIGatewayService) handleChatBufferedStreamingResponse(
	resp *http.Response,
	c *gin.Context,
	originalModel string,
	billingModel string,
	upstreamModel string,
	startTime time.Time,
) (*OpenAIForwardResult, error) {
	requestID := resp.Header.Get("x-request-id")

	scanner := bufio.NewScanner(resp.Body)
	maxLineSize := defaultMaxLineSize
	if s.cfg != nil && s.cfg.Gateway.MaxLineSize > 0 {
		maxLineSize = s.cfg.Gateway.MaxLineSize
	}
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineSize)

	var finalResponse *apicompat.ResponsesResponse
	var usage OpenAIUsage
	acc := apicompat.NewBufferedResponseAccumulator()

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
			continue
		}
		payload := line[6:]

		var event apicompat.ResponsesStreamEvent
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			logger.L().Warn("openai chat_completions buffered: failed to parse event",
				zap.Error(err),
				zap.String("request_id", requestID),
			)
			continue
		}

		// Accumulate delta content for fallback when terminal output is empty.
		acc.ProcessEvent(&event)

		if (event.Type == "response.completed" || event.Type == "response.done" ||
			event.Type == "response.incomplete" || event.Type == "response.failed") &&
			event.Response != nil {
			finalResponse = event.Response
			if event.Response.Usage != nil {
				usage = OpenAIUsage{
					InputTokens:  event.Response.Usage.InputTokens,
					OutputTokens: event.Response.Usage.OutputTokens,
				}
				if event.Response.Usage.InputTokensDetails != nil {
					usage.CacheReadInputTokens = event.Response.Usage.InputTokensDetails.CachedTokens
				}
			}
		}
	}

	if err := scanner.Err(); err != nil {
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			logger.L().Warn("openai chat_completions buffered: read error",
				zap.Error(err),
				zap.String("request_id", requestID),
			)
		}
	}

	if finalResponse == nil {
		writeChatCompletionsError(c, http.StatusBadGateway, "api_error", "Upstream stream ended without a terminal response event")
		return nil, fmt.Errorf("upstream stream ended without terminal event")
	}

	// When the terminal event has an empty output array, reconstruct from
	// accumulated delta events so the client receives the full content.
	acc.SupplementResponseOutput(finalResponse)

	chatResp := apicompat.ResponsesToChatCompletions(finalResponse, originalModel)

	if s.responseHeaderFilter != nil {
		responseheaders.WriteFilteredHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
	}
	c.JSON(http.StatusOK, chatResp)

	return &OpenAIForwardResult{
		RequestID:     requestID,
		Usage:         usage,
		Model:         originalModel,
		BillingModel:  billingModel,
		UpstreamModel: upstreamModel,
		Stream:        false,
		Duration:      time.Since(startTime),
	}, nil
}

// handleChatStreamingResponse reads Responses SSE events from upstream,
// converts each to Chat Completions SSE chunks, and writes them to the client.
func (s *OpenAIGatewayService) handleChatStreamingResponse(
	resp *http.Response,
	c *gin.Context,
	originalModel string,
	billingModel string,
	upstreamModel string,
	includeUsage bool,
	startTime time.Time,
) (*OpenAIForwardResult, error) {
	requestID := resp.Header.Get("x-request-id")

	if s.responseHeaderFilter != nil {
		responseheaders.WriteFilteredHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
	}
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	c.Writer.WriteHeader(http.StatusOK)

	state := apicompat.NewResponsesEventToChatState()
	state.Model = originalModel
	state.IncludeUsage = includeUsage

	var usage OpenAIUsage
	var firstTokenMs *int
	firstChunk := true

	scanner := bufio.NewScanner(resp.Body)
	maxLineSize := defaultMaxLineSize
	if s.cfg != nil && s.cfg.Gateway.MaxLineSize > 0 {
		maxLineSize = s.cfg.Gateway.MaxLineSize
	}
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineSize)

	resultWithUsage := func() *OpenAIForwardResult {
		return &OpenAIForwardResult{
			RequestID:     requestID,
			Usage:         usage,
			Model:         originalModel,
			BillingModel:  billingModel,
			UpstreamModel: upstreamModel,
			Stream:        true,
			Duration:      time.Since(startTime),
			FirstTokenMs:  firstTokenMs,
		}
	}

	processDataLine := func(payload string) bool {
		if firstChunk {
			firstChunk = false
			ms := int(time.Since(startTime).Milliseconds())
			firstTokenMs = &ms
		}

		var event apicompat.ResponsesStreamEvent
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			logger.L().Warn("openai chat_completions stream: failed to parse event",
				zap.Error(err),
				zap.String("request_id", requestID),
			)
			return false
		}

		// Extract usage from completion events
		if (event.Type == "response.completed" || event.Type == "response.incomplete" || event.Type == "response.failed") &&
			event.Response != nil && event.Response.Usage != nil {
			usage = OpenAIUsage{
				InputTokens:  event.Response.Usage.InputTokens,
				OutputTokens: event.Response.Usage.OutputTokens,
			}
			if event.Response.Usage.InputTokensDetails != nil {
				usage.CacheReadInputTokens = event.Response.Usage.InputTokensDetails.CachedTokens
			}
		}

		chunks := apicompat.ResponsesEventToChatChunks(&event, state)
		for _, chunk := range chunks {
			sse, err := apicompat.ChatChunkToSSE(chunk)
			if err != nil {
				logger.L().Warn("openai chat_completions stream: failed to marshal chunk",
					zap.Error(err),
					zap.String("request_id", requestID),
				)
				continue
			}
			if _, err := fmt.Fprint(c.Writer, sse); err != nil {
				logger.L().Info("openai chat_completions stream: client disconnected",
					zap.String("request_id", requestID),
				)
				return true
			}
		}
		if len(chunks) > 0 {
			c.Writer.Flush()
		}
		return false
	}

	finalizeStream := func() (*OpenAIForwardResult, error) {
		if finalChunks := apicompat.FinalizeResponsesChatStream(state); len(finalChunks) > 0 {
			for _, chunk := range finalChunks {
				sse, err := apicompat.ChatChunkToSSE(chunk)
				if err != nil {
					continue
				}
				fmt.Fprint(c.Writer, sse) //nolint:errcheck
			}
		}
		// Send [DONE] sentinel
		fmt.Fprint(c.Writer, "data: [DONE]\n\n") //nolint:errcheck
		c.Writer.Flush()
		return resultWithUsage(), nil
	}

	handleScanErr := func(err error) {
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			logger.L().Warn("openai chat_completions stream: read error",
				zap.Error(err),
				zap.String("request_id", requestID),
			)
		}
	}

	// Determine keepalive interval
	keepaliveInterval := time.Duration(0)
	if s.cfg != nil && s.cfg.Gateway.StreamKeepaliveInterval > 0 {
		keepaliveInterval = time.Duration(s.cfg.Gateway.StreamKeepaliveInterval) * time.Second
	}

	// No keepalive: fast synchronous path
	if keepaliveInterval <= 0 {
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
				continue
			}
			if processDataLine(line[6:]) {
				return resultWithUsage(), nil
			}
		}
		handleScanErr(scanner.Err())
		return finalizeStream()
	}

	// With keepalive: goroutine + channel + select
	type scanEvent struct {
		line string
		err  error
	}
	events := make(chan scanEvent, 16)
	done := make(chan struct{})
	sendEvent := func(ev scanEvent) bool {
		select {
		case events <- ev:
			return true
		case <-done:
			return false
		}
	}
	go func() {
		defer close(events)
		for scanner.Scan() {
			if !sendEvent(scanEvent{line: scanner.Text()}) {
				return
			}
		}
		if err := scanner.Err(); err != nil {
			_ = sendEvent(scanEvent{err: err})
		}
	}()
	defer close(done)

	keepaliveTicker := time.NewTicker(keepaliveInterval)
	defer keepaliveTicker.Stop()
	lastDataAt := time.Now()

	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return finalizeStream()
			}
			if ev.err != nil {
				handleScanErr(ev.err)
				return finalizeStream()
			}
			lastDataAt = time.Now()
			line := ev.line
			if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
				continue
			}
			if processDataLine(line[6:]) {
				return resultWithUsage(), nil
			}

		case <-keepaliveTicker.C:
			if time.Since(lastDataAt) < keepaliveInterval {
				continue
			}
			// Send SSE comment as keepalive
			if _, err := fmt.Fprint(c.Writer, ":\n\n"); err != nil {
				logger.L().Info("openai chat_completions stream: client disconnected during keepalive",
					zap.String("request_id", requestID),
				)
				return resultWithUsage(), nil
			}
			c.Writer.Flush()
		}
	}
}

func buildOpenAIChatCompletionsURL(base string) string {
	normalized := strings.TrimRight(strings.TrimSpace(base), "/")
	if normalized == "" {
		return "https://api.openai.com/v1/chat/completions"
	}
	if strings.HasSuffix(normalized, "/chat/completions") {
		return normalized
	}
	if strings.HasSuffix(normalized, "/v1") {
		return normalized + "/chat/completions"
	}
	if strings.Contains(normalized, "api.openai.com") {
		return normalized + "/v1/chat/completions"
	}
	return normalized + "/chat/completions"
}

func (s *OpenAIGatewayService) forwardNativeAPIKeyChatCompletions(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	originalModel, billingModel, upstreamModel string,
	clientStream bool,
	startTime time.Time,
) (*OpenAIForwardResult, error) {
	upstreamBody := body
	if upstreamModel != originalModel {
		if replaced, err := sjson.SetBytes(body, "model", upstreamModel); err == nil {
			upstreamBody = replaced
		}
	}

	token, _, err := s.GetAccessToken(ctx, account)
	if err != nil {
		return nil, fmt.Errorf("get access token: %w", err)
	}

	targetURL := buildOpenAIChatCompletionsURL(account.GetOpenAIBaseURL())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(upstreamBody))
	if err != nil {
		return nil, fmt.Errorf("build api-key chat request: %w", err)
	}
	ApplySub2APICorrelationHeaders(req)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	if clientStream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}

	proxyURL := ""
	if account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	resp, err := s.httpUpstream.Do(req, proxyURL, account.ID, account.Concurrency)
	if err != nil {
		safeErr := sanitizeUpstreamErrorMessage(err.Error())
		setOpsUpstreamError(c, 0, safeErr, "")
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			Platform: account.Platform, AccountID: account.ID, AccountName: account.Name,
			UpstreamStatusCode: 0, Kind: "request_error", Message: safeErr,
		})
		writeChatCompletionsError(c, http.StatusBadGateway, "upstream_error", "Upstream request failed")
		return nil, fmt.Errorf("api-key chat request failed: %s", safeErr)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		_ = resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(respBody))
		upstreamMsg := strings.TrimSpace(extractUpstreamErrorMessage(respBody))
		upstreamMsg = sanitizeUpstreamErrorMessage(upstreamMsg)
		if s.shouldFailoverOpenAIUpstreamResponse(resp.StatusCode, upstreamMsg, respBody) {
			appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
				Platform: account.Platform, AccountID: account.ID, AccountName: account.Name,
				UpstreamStatusCode: resp.StatusCode, Kind: "failover", Message: upstreamMsg,
			})
			if s.rateLimitService != nil {
				s.rateLimitService.HandleUpstreamError(ctx, account, resp.StatusCode, resp.Header, respBody)
			}
			return nil, buildOpenAIUpstreamFailoverError(account, resp.StatusCode, upstreamMsg, respBody)
		}
		return s.handleChatCompletionsErrorResponse(resp, c, account)
	}

	requestID := resp.Header.Get("x-request-id")
	if s.responseHeaderFilter != nil {
		responseheaders.WriteFilteredHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
	}

	if clientStream {
		c.Writer.Header().Set("Content-Type", "text/event-stream")
		c.Writer.Header().Set("Cache-Control", "no-cache")
		c.Writer.Header().Set("Connection", "keep-alive")
		c.Writer.Header().Set("X-Accel-Buffering", "no")
		c.Writer.WriteHeader(http.StatusOK)

		var usage OpenAIUsage
		var firstTokenMs *int
		firstChunk := true

		scanner := bufio.NewScanner(resp.Body)
		maxLineSize := defaultMaxLineSize
		if s.cfg != nil && s.cfg.Gateway.MaxLineSize > 0 {
			maxLineSize = s.cfg.Gateway.MaxLineSize
		}
		scanner.Buffer(make([]byte, 0, 64*1024), maxLineSize)
		for scanner.Scan() {
			line := scanner.Text()
			if firstChunk && strings.HasPrefix(line, "data:") {
				firstChunk = false
				ms := int(time.Since(startTime).Milliseconds())
				firstTokenMs = &ms
			}
			if line == "data: [DONE]" {
				fmt.Fprint(c.Writer, line+"\n\n") //nolint:errcheck
				c.Writer.Flush()
				break
			}
			if strings.HasPrefix(line, "data: ") {
				payload := line[6:]
				var chunk struct {
					Usage *struct {
						PromptTokens     int `json:"prompt_tokens"`
						CompletionTokens int `json:"completion_tokens"`
					} `json:"usage"`
				}
				if json.Unmarshal([]byte(payload), &chunk) == nil && chunk.Usage != nil {
					usage.InputTokens = chunk.Usage.PromptTokens
					usage.OutputTokens = chunk.Usage.CompletionTokens
				}
			}
			fmt.Fprint(c.Writer, line+"\n") //nolint:errcheck
			if line == "" {
				c.Writer.Flush()
			}
		}
		if err := scanner.Err(); err != nil && !errors.Is(err, context.Canceled) {
			logger.L().Warn("api-key chat_completions stream: read error", zap.Error(err))
		}
		return &OpenAIForwardResult{
			RequestID: requestID, Usage: usage, Model: originalModel,
			BillingModel: billingModel, UpstreamModel: upstreamModel,
			Stream: true, Duration: time.Since(startTime), FirstTokenMs: firstTokenMs,
		}, nil
	}

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		writeChatCompletionsError(c, http.StatusBadGateway, "upstream_error", "Failed to read response")
		return nil, fmt.Errorf("read api-key chat response: %w", err)
	}

	var usage OpenAIUsage
	var parsed struct {
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(respBody, &parsed) == nil && parsed.Usage != nil {
		usage.InputTokens = parsed.Usage.PromptTokens
		usage.OutputTokens = parsed.Usage.CompletionTokens
	}

	c.Data(http.StatusOK, "application/json", respBody)
	return &OpenAIForwardResult{
		RequestID: requestID, Usage: usage, Model: originalModel,
		BillingModel: billingModel, UpstreamModel: upstreamModel,
		Stream: false, Duration: time.Since(startTime),
	}, nil
}

// forwardCopilotChatCompletions sends a Chat Completions request directly to the Copilot API
// without converting to Responses format, which Copilot does not support for all models.
func (s *OpenAIGatewayService) forwardCopilotChatCompletions(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	originalModel, billingModel, upstreamModel string,
	clientStream, includeUsage bool,
	startTime time.Time,
) (*OpenAIForwardResult, error) {
	// Replace model in body with upstream model if different.
	upstreamBody := body
	if upstreamModel != originalModel {
		if replaced, err := sjson.SetBytes(body, "model", upstreamModel); err == nil {
			upstreamBody = replaced
		}
	}

	token, _, err := s.GetAccessToken(ctx, account)
	if err != nil {
		return nil, fmt.Errorf("get access token: %w", err)
	}

	baseURL := account.GetOpenAIBaseURL() // "https://api.githubcopilot.com"
	targetURL := strings.TrimRight(baseURL, "/") + "/chat/completions"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(upstreamBody))
	if err != nil {
		return nil, fmt.Errorf("build copilot request: %w", err)
	}
	ApplySub2APICorrelationHeaders(req)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Editor-Version", "vscode/1.99.3")
	req.Header.Set("Editor-Plugin-Version", "copilot-chat/0.26.7")
	req.Header.Set("Copilot-Integration-Id", "vscode-chat")
	if clientStream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}

	proxyURL := ""
	if account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	resp, err := s.httpUpstream.Do(req, proxyURL, account.ID, account.Concurrency)
	if err != nil {
		safeErr := sanitizeUpstreamErrorMessage(err.Error())
		setOpsUpstreamError(c, 0, safeErr, "")
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			Platform: account.Platform, AccountID: account.ID, AccountName: account.Name,
			UpstreamStatusCode: 0, Kind: "request_error", Message: safeErr,
		})
		writeChatCompletionsError(c, http.StatusBadGateway, "upstream_error", "Upstream request failed")
		return nil, fmt.Errorf("copilot request failed: %s", safeErr)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		_ = resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(respBody))
		upstreamMsg := strings.TrimSpace(extractUpstreamErrorMessage(respBody))
		upstreamMsg = sanitizeUpstreamErrorMessage(upstreamMsg)
		if s.shouldFailoverOpenAIUpstreamResponse(resp.StatusCode, upstreamMsg, respBody) {
			appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
				Platform: account.Platform, AccountID: account.ID, AccountName: account.Name,
				UpstreamStatusCode: resp.StatusCode, Kind: "failover", Message: upstreamMsg,
			})
			if s.rateLimitService != nil {
				s.rateLimitService.HandleUpstreamError(ctx, account, resp.StatusCode, resp.Header, respBody)
			}
			return nil, buildOpenAIUpstreamFailoverError(account, resp.StatusCode, upstreamMsg, respBody)
		}
		return s.handleChatCompletionsErrorResponse(resp, c, account)
	}

	requestID := resp.Header.Get("x-request-id")
	if s.responseHeaderFilter != nil {
		responseheaders.WriteFilteredHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
	}

	if clientStream {
		// Stream SSE chunks directly — Copilot returns standard chat.completion.chunk format.
		c.Writer.Header().Set("Content-Type", "text/event-stream")
		c.Writer.Header().Set("Cache-Control", "no-cache")
		c.Writer.Header().Set("Connection", "keep-alive")
		c.Writer.Header().Set("X-Accel-Buffering", "no")
		c.Writer.WriteHeader(http.StatusOK)

		var usage OpenAIUsage
		var firstTokenMs *int
		firstChunk := true

		scanner := bufio.NewScanner(resp.Body)
		maxLineSize := defaultMaxLineSize
		if s.cfg != nil && s.cfg.Gateway.MaxLineSize > 0 {
			maxLineSize = s.cfg.Gateway.MaxLineSize
		}
		scanner.Buffer(make([]byte, 0, 64*1024), maxLineSize)
		for scanner.Scan() {
			line := scanner.Text()
			if firstChunk && strings.HasPrefix(line, "data:") {
				firstChunk = false
				ms := int(time.Since(startTime).Milliseconds())
				firstTokenMs = &ms
			}
			if line == "data: [DONE]" {
				fmt.Fprint(c.Writer, line+"\n\n") //nolint:errcheck
				c.Writer.Flush()
				break
			}
			if strings.HasPrefix(line, "data: ") {
				// Extract usage from the final chunk if present.
				payload := line[6:]
				var chunk struct {
					Usage *struct {
						PromptTokens     int `json:"prompt_tokens"`
						CompletionTokens int `json:"completion_tokens"`
					} `json:"usage"`
				}
				if json.Unmarshal([]byte(payload), &chunk) == nil && chunk.Usage != nil {
					usage.InputTokens = chunk.Usage.PromptTokens
					usage.OutputTokens = chunk.Usage.CompletionTokens
				}
			}
			fmt.Fprint(c.Writer, line+"\n") //nolint:errcheck
			if line == "" {
				c.Writer.Flush()
			}
		}
		if err := scanner.Err(); err != nil && !errors.Is(err, context.Canceled) {
			logger.L().Warn("copilot chat_completions stream: read error", zap.Error(err))
		}
		return &OpenAIForwardResult{
			RequestID: requestID, Usage: usage, Model: originalModel,
			BillingModel: billingModel, UpstreamModel: upstreamModel,
			Stream: true, Duration: time.Since(startTime), FirstTokenMs: firstTokenMs,
		}, nil
	}

	// Non-streaming: pass response JSON through directly.
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		writeChatCompletionsError(c, http.StatusBadGateway, "upstream_error", "Failed to read response")
		return nil, fmt.Errorf("read copilot response: %w", err)
	}

	var usage OpenAIUsage
	var parsed struct {
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(respBody, &parsed) == nil && parsed.Usage != nil {
		usage.InputTokens = parsed.Usage.PromptTokens
		usage.OutputTokens = parsed.Usage.CompletionTokens
	}

	c.Data(http.StatusOK, "application/json", respBody)
	return &OpenAIForwardResult{
		RequestID: requestID, Usage: usage, Model: originalModel,
		BillingModel: billingModel, UpstreamModel: upstreamModel,
		Stream: false, Duration: time.Since(startTime),
	}, nil
}

// writeChatCompletionsError writes an error response in OpenAI Chat Completions format.
func writeChatCompletionsError(c *gin.Context, statusCode int, errType, message string) {
	c.JSON(statusCode, gin.H{
		"error": gin.H{
			"type":    errType,
			"message": message,
		},
	})
}

// forwardKiroChatCompletions sends a Chat Completions request to the Kiro API
// after converting from OpenAI format to Kiro generateAssistantResponse format.
func (s *OpenAIGatewayService) forwardKiroChatCompletions(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	originalModel, billingModel, upstreamModel string,
	clientStream, includeUsage bool,
	startTime time.Time,
) (*OpenAIForwardResult, error) {
	region := account.GetExtraString("region")
	if region == "" {
		region = "us-east-1"
	}

	accessToken, _, err := s.GetAccessToken(ctx, account)
	if err != nil {
		return nil, fmt.Errorf("get kiro access token: %w", err)
	}

	kiroReq, err := kiro.ConvertOpenAIToKiro(body, account.GetExtraString("profile_arn"))
	if err != nil {
		return nil, fmt.Errorf("convert request to kiro format: %w", err)
	}

	proxyURL := ""
	if account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	httpClient, clientErr := kiro.NewHTTPClient(proxyURL)
	if clientErr != nil {
		return nil, fmt.Errorf("create kiro http client: %w", clientErr)
	}

	resp, respErr := kiro.GenerateAssistantResponse(ctx, httpClient, region, accessToken, kiroReq)
	if respErr != nil {
		return nil, fmt.Errorf("kiro upstream request: %w", respErr)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		_ = resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(respBody))
		if resp.StatusCode == http.StatusBadRequest && kiroRequestHasModelID(kiroReq) && strings.Contains(string(respBody), "INVALID_MODEL_ID") {
			clearKiroRequestModelID(kiroReq)
			resp, respErr = kiro.GenerateAssistantResponse(ctx, httpClient, region, accessToken, kiroReq)
			if respErr != nil {
				return nil, fmt.Errorf("kiro upstream retry without modelId: %w", respErr)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode < 400 {
				goto kiroResponseOK
			}
			respBody, _ = io.ReadAll(io.LimitReader(resp.Body, 2<<20))
			_ = resp.Body.Close()
			resp.Body = io.NopCloser(bytes.NewReader(respBody))
		}
		upstreamMsg := strings.TrimSpace(string(respBody))
		upstreamMsg = sanitizeUpstreamErrorMessage(upstreamMsg)
		if resp.StatusCode == http.StatusUnauthorized {
			return nil, &UpstreamFailoverError{StatusCode: resp.StatusCode, ResponseBody: respBody}
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			if s.rateLimitService != nil {
				s.rateLimitService.HandleUpstreamError(ctx, account, resp.StatusCode, resp.Header, respBody)
			}
			return nil, &UpstreamFailoverError{StatusCode: resp.StatusCode, ResponseBody: respBody}
		}
		writeChatCompletionsError(c, resp.StatusCode, "upstream_error", upstreamMsg)
		return nil, fmt.Errorf("kiro upstream error: status %d", resp.StatusCode)
	}

kiroResponseOK:
	if clientStream {
		c.Writer.Header().Set("Content-Type", "text/event-stream")
		c.Writer.Header().Set("Cache-Control", "no-cache")
		c.Writer.Header().Set("Connection", "keep-alive")
		c.Writer.Header().Set("X-Accel-Buffering", "no")
		c.Writer.WriteHeader(http.StatusOK)

		eventCh, errCh := kiro.ExtractStreamingEvents(resp.Body)
		toolCollector := kiro.NewToolCallCollectorForStream()
		finishReason := "stop"

		roleChunk, _ := kiro.BuildOpenAIStreamChunk("", originalModel, true, nil)
		fmt.Fprintf(c.Writer, "data: %s\n\n", roleChunk)
		c.Writer.Flush()

		for {
			select {
			case event, ok := <-eventCh:
				if !ok {
					goto kiroStreamDone
				}
				if event.Content != "" {
					chunk, _ := kiro.BuildOpenAIStreamChunk(event.Content, originalModel, false, nil)
					fmt.Fprintf(c.Writer, "data: %s\n\n", chunk)
					c.Writer.Flush()
				}
				if event.ToolUse != nil {
					toolCollector.Add(*event.ToolUse)
				}
			case streamErr, ok := <-errCh:
				if !ok {
					continue
				}
				if streamErr != nil {
					goto kiroStreamDone
				}
			}
		}

	kiroStreamDone:
		if toolCalls := toolCollector.Finish(); len(toolCalls) > 0 {
			finishReason = "tool_calls"
			chunk, _ := kiro.BuildOpenAIStreamToolCallsChunk(toolCalls, originalModel)
			fmt.Fprintf(c.Writer, "data: %s\n\n", chunk)
		}
		finishChunk, _ := kiro.BuildOpenAIStreamChunk("", originalModel, false, &finishReason)
		fmt.Fprintf(c.Writer, "data: %s\n\n", finishChunk)
		fmt.Fprintf(c.Writer, "data: [DONE]\n\n")
		c.Writer.Flush()

		return &OpenAIForwardResult{
			RequestID: "", Model: originalModel,
			BillingModel: billingModel, UpstreamModel: upstreamModel,
			Stream: true, Duration: time.Since(startTime),
		}, nil
	}

	events, parseErr := kiro.ParseEventStream(resp.Body)
	if parseErr != nil {
		return nil, fmt.Errorf("parse kiro event stream: %w", parseErr)
	}

	content := kiro.ExtractAssistantContent(events)
	toolCalls := kiro.ExtractToolCalls(events)
	respBytes, buildErr := kiro.BuildOpenAIResponseWithToolCalls(content, originalModel, 0, toolCalls)
	if buildErr != nil {
		return nil, fmt.Errorf("build openai response: %w", buildErr)
	}

	c.Data(http.StatusOK, "application/json", respBytes)

	var usage OpenAIUsage
	return &OpenAIForwardResult{
		RequestID: "", Usage: usage, Model: originalModel,
		BillingModel: billingModel, UpstreamModel: upstreamModel,
		Stream: false, Duration: time.Since(startTime),
	}, nil
}

func kiroRequestHasModelID(req *kiro.GenerateAssistantResponseRequest) bool {
	if req == nil || req.ConversationState == nil || req.ConversationState.CurrentMessage == nil || req.ConversationState.CurrentMessage.UserInputMessage == nil {
		return false
	}
	return strings.TrimSpace(req.ConversationState.CurrentMessage.UserInputMessage.ModelID) != ""
}

func clearKiroRequestModelID(req *kiro.GenerateAssistantResponseRequest) {
	if req == nil || req.ConversationState == nil {
		return
	}
	if req.ConversationState.CurrentMessage != nil && req.ConversationState.CurrentMessage.UserInputMessage != nil {
		req.ConversationState.CurrentMessage.UserInputMessage.ModelID = ""
	}
	for i := range req.ConversationState.History {
		if req.ConversationState.History[i].UserInputMessage != nil {
			req.ConversationState.History[i].UserInputMessage.ModelID = ""
		}
	}
}
