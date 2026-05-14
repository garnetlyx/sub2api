package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestForwardNativeAPIKeyChatCompletions_RespectsConfiguredUserAgentOverride(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"custom-compatible","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	upstream := &httpUpstreamRecorder{resp: nativeChatTestResponse()}
	svc := &OpenAIGatewayService{httpUpstream: upstream}
	account := &Account{
		ID:       39,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":    "sk-test",
			"base_url":   "https://openai-compatible.example/v1",
			"user_agent": "custom-client/1.0",
		},
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	c.Request.Header.Set("User-Agent", "incoming-client/2.0")

	_, err := svc.forwardNativeAPIKeyChatCompletions(
		context.Background(), c, account, body,
		"custom-compatible", "custom-compatible", "custom-compatible",
		false, time.Now(),
	)

	require.NoError(t, err)
	require.NotNil(t, upstream.lastReq)
	require.Equal(t, "custom-client/1.0", upstream.lastReq.Header.Get("User-Agent"))
}

func TestForwardNativeAPIKeyChatCompletions_ForwardsIncomingUserAgent(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"gpt-compatible","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	upstream := &httpUpstreamRecorder{resp: nativeChatTestResponse()}
	svc := &OpenAIGatewayService{httpUpstream: upstream}
	account := &Account{
		ID:       40,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": "https://openai-compatible.example/v1",
		},
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	c.Request.Header.Set("User-Agent", "opencode/1.14.49 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.13")

	_, err := svc.forwardNativeAPIKeyChatCompletions(
		context.Background(), c, account, body,
		"gpt-compatible", "gpt-compatible", "gpt-compatible",
		false, time.Now(),
	)

	require.NoError(t, err)
	require.NotNil(t, upstream.lastReq)
	require.Equal(t, "opencode/1.14.49 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.13", upstream.lastReq.Header.Get("User-Agent"))
}

func TestForwardNativeAPIKeyChatCompletions_DefaultsUserAgentWhenIncomingMissing(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"gpt-compatible","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	upstream := &httpUpstreamRecorder{resp: nativeChatTestResponse()}
	svc := &OpenAIGatewayService{httpUpstream: upstream}
	account := &Account{
		ID:       41,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": "https://openai-compatible.example/v1",
		},
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	c.Request.Header.Del("User-Agent")

	_, err := svc.forwardNativeAPIKeyChatCompletions(
		context.Background(), c, account, body,
		"gpt-compatible", "gpt-compatible", "gpt-compatible",
		false, time.Now(),
	)

	require.NoError(t, err)
	require.NotNil(t, upstream.lastReq)
	require.Equal(t, directProviderDefaultUserAgent, upstream.lastReq.Header.Get("User-Agent"))
}

func TestResolveOpenAIUpstreamUserAgent(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	c.Request.Header.Set("User-Agent", "codex-tui/0.130.0")

	require.Equal(t, "configured/9.9", resolveOpenAIUpstreamUserAgent(c, &Account{
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"user_agent": " configured/9.9 "},
	}))
	require.Equal(t, "codex-tui/0.130.0", resolveOpenAIUpstreamUserAgent(c, &Account{}))

	c.Request.Header.Del("User-Agent")
	require.Equal(t, directProviderDefaultUserAgent, resolveOpenAIUpstreamUserAgent(c, &Account{}))
	require.Equal(t, directProviderDefaultUserAgent, resolveOpenAIUpstreamUserAgent(nil, nil))
}

func nativeChatTestResponse() *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"x-request-id": []string{"req-test"}},
		Body: io.NopCloser(bytes.NewReader([]byte(
			`{"id":"chatcmpl-test","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
		))),
	}
}
