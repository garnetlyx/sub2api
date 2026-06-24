package service

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestIsVolcengineCodingAccount(t *testing.T) {
	tests := []struct {
		name string
		acct *Account
		want bool
	}{
		{
			name: "nil account",
			acct: nil,
			want: false,
		},
		{
			name: "non-apikey oauth account",
			acct: &Account{Type: AccountTypeOAuth, Platform: PlatformOpenAI, Credentials: map[string]any{"base_url": "https://ark.cn-beijing.volces.com/api/coding/v3"}},
			want: false,
		},
		{
			name: "apikey account on volcengine coding v3",
			acct: &Account{Type: AccountTypeAPIKey, Platform: PlatformOpenAI, Credentials: map[string]any{"base_url": "https://ark.cn-beijing.volces.com/api/coding/v3"}},
			want: true,
		},
		{
			name: "apikey account on volcengine coding anthropic path",
			acct: &Account{Type: AccountTypeAPIKey, Platform: PlatformOpenAI, Credentials: map[string]any{"base_url": "https://ark.cn-beijing.volces.com/api/coding"}},
			want: true,
		},
		{
			name: "apikey account on openai direct",
			acct: &Account{Type: AccountTypeAPIKey, Platform: PlatformOpenAI, Credentials: map[string]any{"base_url": "https://api.openai.com/v1"}},
			want: false,
		},
		{
			name: "apikey account on ocgo aggregator (not volces)",
			acct: &Account{Type: AccountTypeAPIKey, Platform: PlatformOpenAI, Credentials: map[string]any{"base_url": "https://opencode.ai/zen/go/v1"}},
			want: false,
		},
		{
			name: "apikey account no base_url",
			acct: &Account{Type: AccountTypeAPIKey, Platform: PlatformOpenAI, Credentials: map[string]any{}},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isVolcengineCodingAccount(tt.acct); got != tt.want {
				t.Errorf("isVolcengineCodingAccount = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSanitizeBodyForVolcengineCoding_NoOpOnCleanBody(t *testing.T) {
	body := []byte(`{"model":"kimi-k2.6","messages":[{"role":"user","content":"hi"}],"max_tokens":8}`)
	out, changed, err := sanitizeBodyForVolcengineCoding(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if changed {
		t.Errorf("changed = true, want false on clean body")
	}
	if string(out) != string(body) {
		t.Errorf("body mutated on clean input")
	}
}

func TestSanitizeBodyForVolcengineCoding_NoOpWhenToolChoiceAbsent(t *testing.T) {
	body := []byte(`{"model":"kimi-k2.6","messages":[{"role":"user","content":"hi"}],"max_tokens":8,"temperature":0.7}`)
	out, changed, err := sanitizeBodyForVolcengineCoding(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if changed {
		t.Errorf("changed = true, want false (pre-check should skip)")
	}
	if string(out) != string(body) {
		t.Errorf("body mutated despite pre-check")
	}
}

func TestSanitizeBodyForVolcengineCoding_ForcedToolChoiceDowngraded(t *testing.T) {
	body := []byte(`{
		"model":"kimi-k2.6",
		"messages":[{"role":"user","content":"hi"}],
		"max_tokens":16,
		"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object","properties":{}}}}],
		"tool_choice":{"type":"function","function":{"name":"f"}}
	}`)
	out, changed, err := sanitizeBodyForVolcengineCoding(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !changed {
		t.Fatalf("changed = false, want true")
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output not valid JSON: %v", err)
	}
	tc, ok := got["tool_choice"]
	if !ok {
		t.Fatalf("tool_choice missing from output")
	}
	tcStr, ok := tc.(string)
	if !ok || tcStr != "auto" {
		t.Errorf("tool_choice = %v, want \"auto\"", tc)
	}
	if _, ok := got["tools"]; !ok {
		t.Errorf("tools array should be preserved, got removed")
	}
}

func TestSanitizeBodyForVolcengineCoding_StringToolChoicePasses(t *testing.T) {
	for _, tc := range []string{`"auto"`, `"none"`, `"required"`} {
		body := []byte(`{"model":"kimi-k2.6","messages":[{"role":"user","content":"hi"}],"max_tokens":8,"tool_choice":` + tc + `}`)
		out, changed, err := sanitizeBodyForVolcengineCoding(body)
		if err != nil {
			t.Fatalf("unexpected error for tool_choice=%s: %v", tc, err)
		}
		if changed {
			t.Errorf("changed = true for tool_choice=%s, should pass through", tc)
		}
		var got map[string]any
		_ = json.Unmarshal(out, &got)
		if got["tool_choice"] != strings.Trim(tc, `"`) {
			t.Errorf("tool_choice = %v, want %s", got["tool_choice"], tc)
		}
	}
}

func TestSanitizeBodyForVolcengineCoding_AllowedToolsObjectPasses(t *testing.T) {
	body := []byte(`{
		"model":"kimi-k2.6",
		"messages":[{"role":"user","content":"hi"}],
		"max_tokens":8,
		"tool_choice":{"type":"allowed_tools","tools":["a","b"]}
	}`)
	out, changed, err := sanitizeBodyForVolcengineCoding(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if changed {
		t.Errorf("changed = true, want false for allowed_tools object")
	}
	var got map[string]any
	_ = json.Unmarshal(out, &got)
	tc, _ := got["tool_choice"].(map[string]any)
	if tc["type"] != "allowed_tools" {
		t.Errorf("allowed_tools downgraded unexpectedly: %v", got["tool_choice"])
	}
}

func TestSanitizeBodyForVolcengineCoding_ImageURLPreserved(t *testing.T) {
	body := []byte(`{
		"model":"kimi-k2.6",
		"messages":[
			{"role":"user","content":[
				{"type":"text","text":"describe"},
				{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAA"}}
			]}
		],
		"max_tokens":16
	}`)
	out, changed, err := sanitizeBodyForVolcengineCoding(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if changed {
		t.Errorf("changed = true, want false: image_url must be preserved")
	}
	if !strings.Contains(string(out), "image_url") {
		t.Errorf("image_url was stripped — sanitizer must not touch vision content")
	}
}

func TestSanitizeBodyForVolcengineCoding_ThinkingStringPreserved(t *testing.T) {
	body := []byte(`{
		"model":"kimi-k2.6",
		"messages":[{"role":"user","content":"hi"}],
		"max_tokens":8,
		"thinking":"high"
	}`)
	out, changed, err := sanitizeBodyForVolcengineCoding(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if changed {
		t.Errorf("changed = true, want false: thinking is out of scope")
	}
	if !strings.Contains(string(out), `"thinking":"high"`) {
		t.Errorf("thinking mutated unexpectedly: %s", out)
	}
}

func TestSanitizeBodyForVolcengineCoding_CombinedForcedToolChoiceWithImage(t *testing.T) {
	body := []byte(`{
		"model":"kimi-k2.6",
		"messages":[{"role":"user","content":[
			{"type":"text","text":"hi"},
			{"type":"image_url","image_url":{"url":"data:image/png;base64,xxx"}}
		]}],
		"max_tokens":16,
		"tools":[{"type":"function","function":{"name":"f","parameters":{}}}],
		"tool_choice":{"type":"function","function":{"name":"f"}},
		"thinking":"high"
	}`)
	out, changed, err := sanitizeBodyForVolcengineCoding(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !changed {
		t.Fatalf("changed = false, want true")
	}
	s := string(out)
	if !strings.Contains(s, `"tool_choice":"auto"`) {
		t.Errorf("tool_choice not downgraded: %s", s)
	}
	if !strings.Contains(s, "image_url") {
		t.Errorf("image_url should survive, got stripped")
	}
	if !strings.Contains(s, `"thinking":"high"`) {
		t.Errorf("thinking should survive, got stripped")
	}
}

func TestSanitizeBodyForVolcengineCoding_Idempotent(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"hi"}],"tool_choice":{"type":"function","function":{"name":"f"}}}`)
	out1, _, err := sanitizeBodyForVolcengineCoding(body)
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	out2, changed2, err := sanitizeBodyForVolcengineCoding(out1)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if changed2 {
		t.Errorf("second pass should be a no-op (idempotent), but body changed again")
	}
	if string(out1) != string(out2) {
		t.Errorf("idempotency violated")
	}
}

func TestSanitizeBodyForVolcengineCoding_InvalidJSON(t *testing.T) {
	body := []byte(`{"tool_choice": not json}`)
	_, _, err := sanitizeBodyForVolcengineCoding(body)
	if err == nil {
		t.Fatalf("expected error on invalid JSON, got nil")
	}
	if !strings.Contains(err.Error(), "volcengine coding sanitize") {
		t.Errorf("error should be wrapped: %v", err)
	}
}

func TestSanitizeBodyForVolcengineCoding_EmptyBody(t *testing.T) {
	out, changed, err := sanitizeBodyForVolcengineCoding(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if changed {
		t.Errorf("changed = true on empty body")
	}
	if out != nil {
		t.Errorf("expected nil passthrough, got %v", out)
	}
}