package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/util/responseheaders"
	"github.com/gin-gonic/gin"
)

const (
	openAIBridgeEndpointChat      = "/v1/chat/completions"
	openAIBridgeEndpointResponses = "/v1/responses"
	openAIBridgeEndpointMessages  = "/v1/messages"
)

type OpenAIBridgeFallbackError struct {
	BridgePlatform string
	Endpoint       string
	StatusCode     int
	Message        string
}

func (e *OpenAIBridgeFallbackError) Error() string {
	if e == nil {
		return "openai bridge fallback"
	}
	if strings.TrimSpace(e.Message) == "" {
		return fmt.Sprintf("openai bridge fallback: platform=%s endpoint=%s", e.BridgePlatform, e.Endpoint)
	}
	return fmt.Sprintf("openai bridge fallback: platform=%s endpoint=%s: %s", e.BridgePlatform, e.Endpoint, e.Message)
}

func shouldBridgeOpenAICompatibilityError(account *Account, statusCode int, upstreamMsg string) bool {
	if account == nil || statusCode < 400 {
		return false
	}
	msg := strings.ToLower(strings.TrimSpace(upstreamMsg))
	if msg == "" {
		return false
	}
	if strings.Contains(msg, "does not support responses api") {
		return true
	}
	if strings.Contains(msg, "requested model is not supported") {
		return true
	}
	if strings.Contains(msg, "unsupported") && account.IsCopilot() {
		return true
	}
	return false
}

func (s *OpenAIGatewayService) findSchedulableBridgeAccount(ctx context.Context, platform string, name string) (*Account, error) {
	if s == nil || s.accountRepo == nil {
		return nil, fmt.Errorf("account repository unavailable")
	}
	platform = strings.TrimSpace(platform)
	name = strings.TrimSpace(name)
	if platform == "" || name == "" {
		return nil, fmt.Errorf("invalid bridge lookup")
	}

	accounts, err := s.accountRepo.ListSchedulableByPlatform(ctx, platform)
	if err != nil {
		return nil, err
	}
	for i := range accounts {
		if strings.TrimSpace(accounts[i].Name) == name && accounts[i].IsSchedulable() {
			account := accounts[i]
			return &account, nil
		}
	}
	return nil, ErrNoAvailableAccounts
}

func (s *OpenAIGatewayService) FindSchedulableBridgeAccount(ctx context.Context, platform string) (*Account, error) {
	switch strings.TrimSpace(platform) {
	case PlatformAnthropic:
		return s.findSchedulableBridgeAccount(ctx, PlatformAnthropic, "litellm-anthropic-internal")
	case PlatformOpenAI:
		return s.findSchedulableBridgeAccount(ctx, PlatformOpenAI, "litellm-openai-internal")
	default:
		return nil, fmt.Errorf("unsupported bridge platform: %s", platform)
	}
}

func (s *OpenAIGatewayService) FindSchedulableEmbeddingAccount(ctx context.Context, requestedModel string) (*Account, error) {
	for _, name := range []string{"omlx-openai-internal", "litellm-openai-internal"} {
		account, err := s.findSchedulableBridgeAccount(ctx, PlatformOpenAI, name)
		if err != nil || account == nil {
			continue
		}
		if account.IsModelSupported(requestedModel) {
			return account, nil
		}
	}
	return nil, ErrNoAvailableAccounts
}

func replaceModelForBridge(account *Account, body []byte) []byte {
	if account == nil || len(body) == 0 {
		return body
	}
	requestedModel := extractRequestedModelFromBody(body)
	if requestedModel == "" {
		return body
	}
	mappedModel := account.GetMappedModel(requestedModel)
	if strings.TrimSpace(mappedModel) == "" || mappedModel == requestedModel {
		return body
	}
	return ReplaceModelInBody(body, mappedModel)
}

func extractRequestedModelFromBody(body []byte) string {
	type modelCarrier struct {
		Model string `json:"model"`
	}
	var carrier modelCarrier
	if err := jsonUnmarshalNoEscape(body, &carrier); err != nil {
		return ""
	}
	return strings.TrimSpace(carrier.Model)
}

func jsonUnmarshalNoEscape(body []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	return dec.Decode(dst)
}

func (s *OpenAIGatewayService) forwardBridgePassthrough(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	endpoint string,
	body []byte,
) (*OpenAIForwardResult, error) {
	if s == nil || account == nil {
		return nil, fmt.Errorf("bridge passthrough unavailable")
	}

	baseURL := strings.TrimRight(strings.TrimSpace(account.GetBaseURL()), "/")
	if account.UsesOpenAIGateway() {
		baseURL = strings.TrimRight(strings.TrimSpace(account.GetOpenAIBaseURL()), "/")
	}
	if baseURL == "" {
		return nil, fmt.Errorf("bridge account base URL missing")
	}

	reqURL := bridgePassthroughURL(baseURL, endpoint)
	if strings.HasPrefix(endpoint, "/v1/responses") && strings.Contains(c.Request.URL.Path, "/responses") {
		if suffix := responsesPathSuffix(c.Request.URL.Path); suffix != "" {
			reqURL = bridgePassthroughURL(baseURL, openAIBridgeEndpointResponses+suffix)
		}
	}

	body = replaceModelForBridge(account, body)
	requestedModel := extractRequestedModelFromBody(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	ApplySub2APICorrelationHeaders(req)
	req.Header.Set("Content-Type", "application/json")
	copySelectedRequestHeaders(req.Header, c.Request.Header)

	if account.IsAnthropic() {
		req.Header.Set("x-api-key", account.GetCredential("api_key"))
		if strings.TrimSpace(req.Header.Get("anthropic-version")) == "" {
			req.Header.Set("anthropic-version", "2023-06-01")
		}
	} else {
		req.Header.Set("Authorization", "Bearer "+account.GetCredential("api_key"))
	}

	proxyURL := ""
	if account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	resp, err := s.httpUpstream.Do(req, proxyURL, account.ID, account.Concurrency)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if account.IsAnthropic() {
		responseheaders.WriteFilteredHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
	} else {
		writeOpenAIPassthroughResponseHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
	}
	c.Status(resp.StatusCode)
	if _, err := io.Copy(c.Writer, resp.Body); err != nil {
		return nil, err
	}

	return &OpenAIForwardResult{
		Model:           requestedModel,
		BillingModel:    requestedModel,
		ResponseHeaders: resp.Header.Clone(),
	}, nil
}

func (s *OpenAIGatewayService) ForwardBridgePassthrough(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	endpoint string,
	body []byte,
) (*OpenAIForwardResult, error) {
	return s.forwardBridgePassthrough(ctx, c, account, endpoint, body)
}

func copySelectedRequestHeaders(dst http.Header, src http.Header) {
	if dst == nil || src == nil {
		return
	}
	for _, key := range []string{
		"Accept",
		"Accept-Encoding",
		"Accept-Language",
		"Anthropic-Beta",
		"Anthropic-Version",
		"OpenAI-Beta",
		"User-Agent",
		"X-Stainless-Arch",
		"X-Stainless-Helper-Method",
		"X-Stainless-Lang",
		"X-Stainless-Os",
		"X-Stainless-Package-Version",
		"X-Stainless-Retry-Count",
		"X-Stainless-Runtime",
		"X-Stainless-Runtime-Version",
	} {
		for _, value := range src.Values(key) {
			dst.Add(key, value)
		}
	}
}

func responsesPathSuffix(rawPath string) string {
	trimmed := strings.TrimSpace(rawPath)
	if trimmed == "" {
		return ""
	}
	idx := strings.LastIndex(trimmed, "/responses")
	if idx < 0 {
		return ""
	}
	suffix := trimmed[idx+len("/responses"):]
	if suffix == "/" {
		return ""
	}
	return suffix
}

func bridgePassthroughURL(baseURL, endpoint string) string {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	endpoint = "/" + strings.TrimLeft(strings.TrimSpace(endpoint), "/")
	if baseURL == "" {
		return endpoint
	}
	if strings.HasSuffix(baseURL, "/v1") && strings.HasPrefix(endpoint, "/v1/") {
		return baseURL + strings.TrimPrefix(endpoint, "/v1")
	}
	return baseURL + endpoint
}
