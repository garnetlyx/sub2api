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
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/util/responseheaders"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// ForwardAsAnthropic accepts an Anthropic Messages request body, converts it
// to OpenAI Responses API format, forwards to the OpenAI upstream, and converts
// the response back to Anthropic Messages format. This enables Claude Code
// clients to access OpenAI models through the standard /v1/messages endpoint.
func (s *OpenAIGatewayService) ForwardAsAnthropic(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	promptCacheKey string,
	defaultMappedModel string,
) (*OpenAIForwardResult, error) {
	startTime := time.Now()

	// 1. Parse Anthropic request
	var anthropicReq apicompat.AnthropicRequest
	if err := json.Unmarshal(body, &anthropicReq); err != nil {
		return nil, fmt.Errorf("parse anthropic request: %w", err)
	}
	originalModel := anthropicReq.Model
	applyOpenAICompatModelNormalization(&anthropicReq)
	normalizedModel := anthropicReq.Model
	clientStream := anthropicReq.Stream // client's original stream preference

	// 2. Convert Anthropic → Responses
	responsesReq, err := apicompat.AnthropicToResponses(&anthropicReq)
	if err != nil {
		return nil, fmt.Errorf("convert anthropic to responses: %w", err)
	}

	// Upstream always uses streaming (upstream may not support sync mode).
	// The client's original preference determines the response format.
	responsesReq.Stream = true
	isStream := true

	// 2b. Handle BetaFastMode → service_tier: "priority"
	if containsBetaToken(c.GetHeader("anthropic-beta"), claude.BetaFastMode) {
		responsesReq.ServiceTier = "priority"
	}

	// 3. Model mapping
	billingModel := resolveOpenAIForwardModel(account, normalizedModel, defaultMappedModel)
	upstreamModel := normalizeOpenAIModelForUpstream(account, billingModel)
	responsesReq.Model = upstreamModel

	logger.L().Debug("openai messages: model mapping applied",
		zap.Int64("account_id", account.ID),
		zap.String("original_model", originalModel),
		zap.String("normalized_model", normalizedModel),
		zap.String("billing_model", billingModel),
		zap.String("upstream_model", upstreamModel),
		zap.Bool("stream", isStream),
	)

	// 4. Marshal Responses request body, then apply OAuth codex transform
	responsesBody, err := json.Marshal(responsesReq)
	if err != nil {
		return nil, fmt.Errorf("marshal responses request: %w", err)
	}

	if account.Type == AccountTypeOAuth {
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
		// OAuth codex transform forces stream=true upstream, so always use
		// the streaming response handler regardless of what the client asked.
		isStream = true
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
	upstreamReq, err := s.buildUpstreamRequest(ctx, c, account, responsesBody, token, isStream, promptCacheKey, false)
	if err != nil {
		return nil, fmt.Errorf("build upstream request: %w", err)
	}

	// Override session_id with a deterministic UUID derived from the isolated
	// session key, ensuring different API keys produce different upstream sessions.
	if promptCacheKey != "" {
		apiKeyID := getAPIKeyIDFromContext(c)
		upstreamReq.Header.Set("session_id", generateSessionUUID(isolateOpenAISessionID(apiKeyID, promptCacheKey)))
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
		writeAnthropicError(c, http.StatusBadGateway, "api_error", "Upstream request failed")
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
				BridgePlatform: PlatformAnthropic,
				Endpoint:       openAIBridgeEndpointMessages,
				StatusCode:     resp.StatusCode,
				Message:        upstreamMsg,
			}
		}
		// Non-failover error: return Anthropic-formatted error to client
		return s.handleAnthropicErrorResponse(resp, c, account)
	}

	// 9. Handle normal response
	// Upstream is always streaming; choose response format based on client preference.
	var result *OpenAIForwardResult
	var handleErr error
	if clientStream {
		result, handleErr = s.handleAnthropicStreamingResponse(resp, c, originalModel, billingModel, upstreamModel, startTime)
	} else {
		// Client wants JSON: buffer the streaming response and assemble a JSON reply.
		result, handleErr = s.handleAnthropicBufferedStreamingResponse(resp, c, originalModel, billingModel, upstreamModel, startTime)
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

func (s *OpenAIGatewayService) ForwardKiroAnthropicMessages(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	originalModel string,
	stream bool,
) (*ForwardResult, error) {
	startTime := time.Now()
	billingModel := resolveOpenAIForwardModel(account, originalModel, "")
	upstreamModel := normalizeOpenAIModelForUpstream(account, billingModel)

	region := account.GetExtraString("region")
	if region == "" {
		region = "us-east-1"
	}

	accessToken, _, err := s.GetAccessToken(ctx, account)
	if err != nil {
		return nil, fmt.Errorf("get kiro access token: %w", err)
	}

	kiroReq, err := kiro.ConvertAnthropicToKiro(body, account.GetExtraString("profile_arn"))
	if err != nil {
		return nil, fmt.Errorf("convert anthropic request to kiro format: %w", err)
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
				goto kiroMessagesOK
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
		writeAnthropicError(c, resp.StatusCode, "upstream_error", upstreamMsg)
		return nil, fmt.Errorf("kiro upstream error: status %d", resp.StatusCode)
	}

kiroMessagesOK:
	if stream {
		return s.handleKiroAnthropicStreamingResponse(resp.Body, c, originalModel, billingModel, upstreamModel, startTime)
	}
	return s.handleKiroAnthropicBufferedResponse(resp.Body, c, originalModel, billingModel, upstreamModel, startTime)
}

func (s *OpenAIGatewayService) handleKiroAnthropicBufferedResponse(
	body io.Reader,
	c *gin.Context,
	originalModel string,
	billingModel string,
	upstreamModel string,
	startTime time.Time,
) (*ForwardResult, error) {
	events, parseErr := kiro.ParseEventStream(body)
	if parseErr != nil {
		return nil, fmt.Errorf("parse kiro event stream: %w", parseErr)
	}

	content := kiro.ExtractAssistantContent(events)
	toolCalls := kiro.ExtractToolCalls(events)
	respBytes, buildErr := kiro.BuildAnthropicResponse(content, originalModel, toolCalls)
	if buildErr != nil {
		return nil, fmt.Errorf("build anthropic response: %w", buildErr)
	}

	c.Data(http.StatusOK, "application/json", respBytes)
	return &ForwardResult{
		RequestID:     "",
		Usage:         ClaudeUsage{InputTokens: 0, OutputTokens: len(content) / 4},
		Model:         originalModel,
		UpstreamModel: upstreamModel,
		Stream:        false,
		Duration:      time.Since(startTime),
	}, nil
}

func (s *OpenAIGatewayService) handleKiroAnthropicStreamingResponse(
	body io.Reader,
	c *gin.Context,
	originalModel string,
	billingModel string,
	upstreamModel string,
	startTime time.Time,
) (*ForwardResult, error) {
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	c.Writer.WriteHeader(http.StatusOK)

	usage := ClaudeUsage{}
	firstTokenMs := int(time.Since(startTime).Milliseconds())
	messageID := fmt.Sprintf("msg_kiro_%d", time.Now().Unix())
	startEvt := apicompat.AnthropicStreamEvent{
		Type: "message_start",
		Message: &apicompat.AnthropicResponse{
			ID:      messageID,
			Type:    "message",
			Role:    "assistant",
			Content: []apicompat.AnthropicContentBlock{},
			Model:   originalModel,
			Usage:   apicompat.AnthropicUsage{},
		},
	}
	if err := writeAnthropicStreamEvent(c, startEvt); err != nil {
		return &ForwardResult{Model: originalModel, UpstreamModel: upstreamModel, Stream: true, Duration: time.Since(startTime), ClientDisconnect: true, FirstTokenMs: &firstTokenMs}, nil
	}

	eventCh, errCh := kiro.ExtractStreamingEvents(body)
	toolCollector := kiro.NewToolCallCollectorForStream()
	textStarted := false
	textIndex := 0
	outputChars := 0

	for {
		select {
		case event, ok := <-eventCh:
			if !ok {
				goto streamDone
			}
			if event.Content != "" {
				if !textStarted {
					block := apicompat.AnthropicContentBlock{Type: "text", Text: ""}
					if err := writeAnthropicStreamEvent(c, apicompat.AnthropicStreamEvent{Type: "content_block_start", Index: &textIndex, ContentBlock: &block}); err != nil {
						return &ForwardResult{Model: originalModel, UpstreamModel: upstreamModel, Stream: true, Duration: time.Since(startTime), ClientDisconnect: true, FirstTokenMs: &firstTokenMs}, nil
					}
					textStarted = true
				}
				delta := &apicompat.AnthropicDelta{Type: "text_delta", Text: event.Content}
				if err := writeAnthropicStreamEvent(c, apicompat.AnthropicStreamEvent{Type: "content_block_delta", Index: &textIndex, Delta: delta}); err != nil {
					return &ForwardResult{Model: originalModel, UpstreamModel: upstreamModel, Stream: true, Duration: time.Since(startTime), ClientDisconnect: true, FirstTokenMs: &firstTokenMs}, nil
				}
				outputChars += len(event.Content)
			}
			if event.ToolUse != nil {
				toolCollector.Add(*event.ToolUse)
			}
		case streamErr, ok := <-errCh:
			if !ok {
				continue
			}
			if streamErr != nil {
				goto streamDone
			}
		}
	}

streamDone:
	if textStarted {
		if err := writeAnthropicStreamEvent(c, apicompat.AnthropicStreamEvent{Type: "content_block_stop", Index: &textIndex}); err != nil {
			return &ForwardResult{Model: originalModel, UpstreamModel: upstreamModel, Stream: true, Duration: time.Since(startTime), ClientDisconnect: true, FirstTokenMs: &firstTokenMs}, nil
		}
	}

	toolCalls := toolCollector.Finish()
	nextIndex := 0
	if textStarted {
		nextIndex = 1
	}
	for i, block := range kiro.AnthropicBlocksFromToolCalls("", toolCalls) {
		if block.Type != "tool_use" {
			continue
		}
		idx := nextIndex + i
		input := block.Input
		block.Input = json.RawMessage(`{}`)
		if err := writeAnthropicStreamEvent(c, apicompat.AnthropicStreamEvent{Type: "content_block_start", Index: &idx, ContentBlock: &block}); err != nil {
			return &ForwardResult{Model: originalModel, UpstreamModel: upstreamModel, Stream: true, Duration: time.Since(startTime), ClientDisconnect: true, FirstTokenMs: &firstTokenMs}, nil
		}
		delta := &apicompat.AnthropicDelta{Type: "input_json_delta", PartialJSON: string(input)}
		if err := writeAnthropicStreamEvent(c, apicompat.AnthropicStreamEvent{Type: "content_block_delta", Index: &idx, Delta: delta}); err != nil {
			return &ForwardResult{Model: originalModel, UpstreamModel: upstreamModel, Stream: true, Duration: time.Since(startTime), ClientDisconnect: true, FirstTokenMs: &firstTokenMs}, nil
		}
		if err := writeAnthropicStreamEvent(c, apicompat.AnthropicStreamEvent{Type: "content_block_stop", Index: &idx}); err != nil {
			return &ForwardResult{Model: originalModel, UpstreamModel: upstreamModel, Stream: true, Duration: time.Since(startTime), ClientDisconnect: true, FirstTokenMs: &firstTokenMs}, nil
		}
	}

	stopReason := "end_turn"
	if len(toolCalls) > 0 {
		stopReason = "tool_use"
	}
	usage.OutputTokens = outputChars / 4
	msgDelta := apicompat.AnthropicStreamEvent{
		Type:  "message_delta",
		Delta: &apicompat.AnthropicDelta{StopReason: stopReason, StopSequence: nil},
		Usage: &apicompat.AnthropicUsage{OutputTokens: usage.OutputTokens},
	}
	if err := writeAnthropicStreamEvent(c, msgDelta); err != nil {
		return &ForwardResult{Model: originalModel, UpstreamModel: upstreamModel, Stream: true, Duration: time.Since(startTime), ClientDisconnect: true, FirstTokenMs: &firstTokenMs}, nil
	}
	if err := writeAnthropicStreamEvent(c, apicompat.AnthropicStreamEvent{Type: "message_stop"}); err != nil {
		return &ForwardResult{Model: originalModel, UpstreamModel: upstreamModel, Stream: true, Duration: time.Since(startTime), ClientDisconnect: true, FirstTokenMs: &firstTokenMs}, nil
	}

	return &ForwardResult{
		RequestID:     "",
		Usage:         usage,
		Model:         originalModel,
		UpstreamModel: upstreamModel,
		Stream:        true,
		Duration:      time.Since(startTime),
		FirstTokenMs:  &firstTokenMs,
	}, nil
}

func writeAnthropicStreamEvent(c *gin.Context, evt apicompat.AnthropicStreamEvent) error {
	sse, err := apicompat.ResponsesAnthropicEventToSSE(evt)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprint(c.Writer, sse); err != nil {
		return err
	}
	c.Writer.Flush()
	return nil
}

// handleAnthropicErrorResponse reads an upstream error and returns it in
// Anthropic error format.
func (s *OpenAIGatewayService) handleAnthropicErrorResponse(
	resp *http.Response,
	c *gin.Context,
	account *Account,
) (*OpenAIForwardResult, error) {
	return s.handleCompatErrorResponse(resp, c, account, writeAnthropicError)
}

// handleAnthropicBufferedStreamingResponse reads all Responses SSE events from
// the upstream streaming response, finds the terminal event (response.completed
// / response.incomplete / response.failed), converts the complete response to
// Anthropic Messages JSON format, and writes it to the client.
// This is used when the client requested stream=false but the upstream is always
// streaming.
func (s *OpenAIGatewayService) handleAnthropicBufferedStreamingResponse(
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

	for scanner.Scan() {
		line := scanner.Text()

		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
			continue
		}
		payload := line[6:]

		var event apicompat.ResponsesStreamEvent
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			logger.L().Warn("openai messages buffered: failed to parse event",
				zap.Error(err),
				zap.String("request_id", requestID),
			)
			continue
		}

		// Terminal events carry the complete ResponsesResponse with output + usage.
		if (event.Type == "response.completed" || event.Type == "response.incomplete" || event.Type == "response.failed") &&
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
			logger.L().Warn("openai messages buffered: read error",
				zap.Error(err),
				zap.String("request_id", requestID),
			)
		}
	}

	if finalResponse == nil {
		writeAnthropicError(c, http.StatusBadGateway, "api_error", "Upstream stream ended without a terminal response event")
		return nil, fmt.Errorf("upstream stream ended without terminal event")
	}

	anthropicResp := apicompat.ResponsesToAnthropic(finalResponse, originalModel)

	if s.responseHeaderFilter != nil {
		responseheaders.WriteFilteredHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
	}
	c.JSON(http.StatusOK, anthropicResp)

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

// handleAnthropicStreamingResponse reads Responses SSE events from upstream,
// converts each to Anthropic SSE events, and writes them to the client.
// When StreamKeepaliveInterval is configured, it uses a goroutine + channel
// pattern to send Anthropic ping events during periods of upstream silence,
// preventing proxy/client timeout disconnections.
func (s *OpenAIGatewayService) handleAnthropicStreamingResponse(
	resp *http.Response,
	c *gin.Context,
	originalModel string,
	billingModel string,
	upstreamModel string,
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

	state := apicompat.NewResponsesEventToAnthropicState()
	state.Model = originalModel
	var usage OpenAIUsage
	var firstTokenMs *int
	firstChunk := true

	scanner := bufio.NewScanner(resp.Body)
	maxLineSize := defaultMaxLineSize
	if s.cfg != nil && s.cfg.Gateway.MaxLineSize > 0 {
		maxLineSize = s.cfg.Gateway.MaxLineSize
	}
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineSize)

	// resultWithUsage builds the final result snapshot.
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

	// processDataLine handles a single "data: ..." SSE line from upstream.
	// Returns (clientDisconnected bool).
	processDataLine := func(payload string) bool {
		if firstChunk {
			firstChunk = false
			ms := int(time.Since(startTime).Milliseconds())
			firstTokenMs = &ms
		}

		var event apicompat.ResponsesStreamEvent
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			logger.L().Warn("openai messages stream: failed to parse event",
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

		// Convert to Anthropic events
		events := apicompat.ResponsesEventToAnthropicEvents(&event, state)
		for _, evt := range events {
			sse, err := apicompat.ResponsesAnthropicEventToSSE(evt)
			if err != nil {
				logger.L().Warn("openai messages stream: failed to marshal event",
					zap.Error(err),
					zap.String("request_id", requestID),
				)
				continue
			}
			if _, err := fmt.Fprint(c.Writer, sse); err != nil {
				logger.L().Info("openai messages stream: client disconnected",
					zap.String("request_id", requestID),
				)
				return true
			}
		}
		if len(events) > 0 {
			c.Writer.Flush()
		}
		return false
	}

	// finalizeStream sends any remaining Anthropic events and returns the result.
	finalizeStream := func() (*OpenAIForwardResult, error) {
		if finalEvents := apicompat.FinalizeResponsesAnthropicStream(state); len(finalEvents) > 0 {
			for _, evt := range finalEvents {
				sse, err := apicompat.ResponsesAnthropicEventToSSE(evt)
				if err != nil {
					continue
				}
				fmt.Fprint(c.Writer, sse) //nolint:errcheck
			}
			c.Writer.Flush()
		}
		return resultWithUsage(), nil
	}

	// handleScanErr logs scanner errors if meaningful.
	handleScanErr := func(err error) {
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			logger.L().Warn("openai messages stream: read error",
				zap.Error(err),
				zap.String("request_id", requestID),
			)
		}
	}

	// ── Determine keepalive interval ──
	keepaliveInterval := time.Duration(0)
	if s.cfg != nil && s.cfg.Gateway.StreamKeepaliveInterval > 0 {
		keepaliveInterval = time.Duration(s.cfg.Gateway.StreamKeepaliveInterval) * time.Second
	}

	// ── No keepalive: fast synchronous path (no goroutine overhead) ──
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

	// ── With keepalive: goroutine + channel + select ──
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
				// Upstream closed
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
			// Send Anthropic-format ping event
			if _, err := fmt.Fprint(c.Writer, "event: ping\ndata: {\"type\":\"ping\"}\n\n"); err != nil {
				// Client disconnected
				logger.L().Info("openai messages stream: client disconnected during keepalive",
					zap.String("request_id", requestID),
				)
				return resultWithUsage(), nil
			}
			c.Writer.Flush()
		}
	}
}

// writeAnthropicError writes an error response in Anthropic Messages API format.
func writeAnthropicError(c *gin.Context, statusCode int, errType, message string) {
	c.JSON(statusCode, gin.H{
		"type": "error",
		"error": gin.H{
			"type":    errType,
			"message": message,
		},
	})
}
