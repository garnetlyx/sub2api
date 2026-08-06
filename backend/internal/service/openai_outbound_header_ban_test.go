package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// bannedSub2APIHeaders enumerates every X-Sub2API-* header that must NEVER
// appear on an outbound request to any upstream provider. Attaching them
// exposes the gateway and triggers upstream anti-abuse responses
// (e.g. ChatGPT account logout / refresh-token revocation).
var bannedSub2APIHeaders = []string{
	"X-Sub2API-Request-Id",
	"X-Sub2API-Client-Request-Id",
	"X-Sub2API-Trace-Origin",
}

func assertNoBannedHeaders(t *testing.T, h http.Header) {
	t.Helper()
	for _, banned := range bannedSub2APIHeaders {
		if v := h.Get(banned); v != "" {
			t.Fatalf("banned outbound header %q leaked to upstream with value %q", banned, v)
		}
	}
	for k := range h {
		if strings.HasPrefix(strings.ToLower(k), "x-sub2api") {
			t.Fatalf("banned outbound header prefix leak: %q=%q", k, h.Get(k))
		}
	}
}

// runOAuthCodexForward drives the full Forward path for an OpenAI OAuth
// (non-Copilot) account with codex_cli_rs User-Agent, returning the recorded
// outbound request and body so assertions can run against the exact bytes
// sub2api places on the wire to chatgpt.com/backend-api/codex/responses.
func runOAuthCodexForward(t *testing.T, passthrough bool, requestBody []byte) (*http.Request, []byte) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(nil))
	c.Request.Header.Set("User-Agent", "codex_cli_rs/0.144.1 (Mac OS 14.0; arm64) xterm-256color")
	c.Request.Header.Set("originator", "codex_cli_rs")

	// Minimal SSE response so Forward completes without error.
	upstreamSSE := strings.Join([]string{
		`data: {"type":"response.output_text.delta","delta":"OK"}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(upstreamSSE)),
	}
	upstream := &httpUpstreamRecorder{resp: resp}

	svc := &OpenAIGatewayService{
		cfg:          &config.Config{Gateway: config.GatewayConfig{ForceCodexCLI: false}},
		httpUpstream: upstream,
	}

	extra := map[string]any{}
	if passthrough {
		extra["openai_passthrough"] = true
	}
	account := &Account{
		ID:             42,
		Name:           "codex-oauth-test",
		Platform:       PlatformOpenAI,
		Type:           AccountTypeOAuth,
		Concurrency:    1,
		Credentials:    map[string]any{"access_token": "oauth-token", "chatgpt_account_id": "chatgpt-acc"},
		Extra:          extra,
		Status:         StatusActive,
		Schedulable:    true,
		RateMultiplier: f64p(1),
	}

	result, err := svc.Forward(context.Background(), c, account, requestBody)
	require.NoError(t, err)
	require.NotNil(t, result)

	require.NotNil(t, upstream.lastReq, "outbound request was not recorded")
	return upstream.lastReq, upstream.lastBody
}

// TestOutboundNoBannedSub2APIHeaders_Passthrough verifies the X-Sub2API-*
// ban holds on the passthrough (openai_passthrough=true) codex OAuth path.
func TestOutboundNoBannedSub2APIHeaders_Passthrough(t *testing.T) {
	body := []byte(`{"model":"gpt-5.2","stream":true,"store":true,"input":[{"type":"text","text":"hi"}]}`)
	req, _ := runOAuthCodexForward(t, true, body)
	assertNoBannedHeaders(t, req.Header)
}

// TestOutboundNoBannedSub2APIHeaders_NonPassthrough verifies the X-Sub2API-*
// ban holds on the legacy (applyCodexOAuthTransform) codex OAuth path.
func TestOutboundNoBannedSub2APIHeaders_NonPassthrough(t *testing.T) {
	body := []byte(`{"model":"gpt-5.2","stream":true,"store":true,"input":[{"type":"text","text":"hi"}]}`)
	req, _ := runOAuthCodexForward(t, false, body)
	assertNoBannedHeaders(t, req.Header)
}

// TestOutboundVersionHeaderPresent_Passthrough asserts codex's required
// `version` header is now present on the default (non-compact) Responses path
// of the passthrough builder. Previously it was only set on the compact path.
func TestOutboundVersionHeaderPresent_Passthrough(t *testing.T) {
	body := []byte(`{"model":"gpt-5.2","stream":true,"store":true,"input":[{"type":"text","text":"hi"}]}`)
	req, _ := runOAuthCodexForward(t, true, body)
	require.Equal(t, codexCLIVersion, req.Header.Get("version"),
		"version header must be set on the default Responses path, matching codex CLI")
}

// TestOutboundVersionHeaderPresent_NonPassthrough asserts the same on the
// legacy (non-passthrough) codex OAuth builder.
func TestOutboundVersionHeaderPresent_NonPassthrough(t *testing.T) {
	body := []byte(`{"model":"gpt-5.2","stream":true,"store":true,"input":[{"type":"text","text":"hi"}]}`)
	req, _ := runOAuthCodexForward(t, false, body)
	require.Equal(t, codexCLIVersion, req.Header.Get("version"))
}

// TestReasoningEffortMinimalPreserved verifies sub2api no longer rewrites
// reasoning.effort from "minimal" to "none". codex accepts "minimal" and the
// silent rewrite diverged from the client's intent.
func TestReasoningEffortMinimalPreserved(t *testing.T) {
	body := []byte(`{"model":"gpt-5.2","stream":true,"store":true,"reasoning":{"effort":"minimal"},"input":[{"type":"text","text":"hi"}]}`)
	_, outboundBody := runOAuthCodexForward(t, false, body)
	require.Equal(t, "minimal", gjson.GetBytes(outboundBody, "reasoning.effort").String(),
		"reasoning.effort must be forwarded verbatim; the minimal->none rewrite has been removed")
}

// TestReasoningEffortMinimalPreserved_Passthrough repeats the assertion on
// the passthrough path.
func TestReasoningEffortMinimalPreserved_Passthrough(t *testing.T) {
	body := []byte(`{"model":"gpt-5.2","stream":true,"store":true,"reasoning":{"effort":"minimal"},"input":[{"type":"text","text":"hi"}]}`)
	_, outboundBody := runOAuthCodexForward(t, true, body)
	require.Equal(t, "minimal", gjson.GetBytes(outboundBody, "reasoning.effort").String())
}
