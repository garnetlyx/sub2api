//go:build unit

package service

import (
	"testing"
	"time"
)

func TestAccountKind(t *testing.T) {
	tests := []struct {
		name string
		acct *Account
		want string
	}{
		{"nil account is empty", nil, ""},
		{
			"oauth openai is oauth-managed",
			&Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Name: "li-p"},
			"oauth-managed",
		},
		{
			"setup-token anthropic is oauth-managed",
			&Account{Platform: PlatformAnthropic, Type: AccountTypeSetupToken, Name: "anthropic-pool-1"},
			"oauth-managed",
		},
		{
			"copilot oauth is oauth-managed",
			&Account{Platform: PlatformCopilot, Type: AccountTypeOAuth, Name: "copilot-1"},
			"oauth-managed",
		},
		{
			"omlx internal apikey is runtime-local",
			&Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Name: "omlx-openai-internal",
				Credentials: map[string]any{"base_url": "http://127.0.0.1:8000/v1"}},
			"runtime-local",
		},
		{
			"ds4 internal apikey is runtime-local",
			&Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Name: "ds4-openai-internal",
				Credentials: map[string]any{"base_url": "http://127.0.0.1:8001/v1"}},
			"runtime-local",
		},
		{
			"litellm bridge by name is bridge-internal",
			&Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Name: InternalBridgeOpenAIAccountName,
				Credentials: map[string]any{"base_url": "http://127.0.0.1:4001/v1"}},
			"bridge-internal",
		},
		{
			"third-party apikey is provider-direct",
			&Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Name: "proxy-openai-deepseek",
				Credentials: map[string]any{"base_url": "https://api.deepseek.com"}},
			"provider-direct",
		},
		{
			"tailscale runtime node is runtime-local",
			&Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Name: "proxy-omlx-m3u",
				Credentials: map[string]any{"base_url": "http://100.94.125.103:8000/v1"}},
			"runtime-local",
		},
		{
			"unknown type is other",
			&Account{Platform: PlatformOpenAI, Type: "bedrock", Name: "weird"},
			"other",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.acct.AccountKind()
			if got != tt.want {
				t.Errorf("AccountKind() = %q, want %q (provider_identity=%q)", got, tt.want, tt.acct.ProviderIdentity())
			}
		})
	}
}

func TestAssessSchedulingState(t *testing.T) {
	now := mustParseTime(t, "2026-08-06T18:00:00-07:00")
	past := mustParseTime(t, "2026-08-01T00:00:00-07:00")
	future := mustParseTime(t, "2026-12-31T00:00:00-07:00")

	tests := []struct {
		name      string
		acc       *Account
		wantState string
		wantBlock BlockedByReason
	}{
		{
			"active and schedulable",
			&Account{Status: StatusActive, Schedulable: true},
			"active", BlockedByNone,
		},
		{
			"error status dominates",
			&Account{Status: StatusError, Schedulable: true, RateLimitResetAt: future},
			"error", BlockedByError,
		},
		{
			"rate limit in future",
			&Account{Status: StatusActive, Schedulable: true, RateLimitResetAt: future},
			"rate_limited", BlockedByRateLimit,
		},
		{
			"rate limit in past is not blocked",
			&Account{Status: StatusActive, Schedulable: true, RateLimitResetAt: past},
			"active", BlockedByNone,
		},
		{
			"overload in future",
			&Account{Status: StatusActive, Schedulable: true, OverloadUntil: future},
			"overloaded", BlockedByOverloaded,
		},
		{
			"temp unschedulable",
			&Account{Status: StatusActive, Schedulable: true, TempUnschedulableUntil: future},
			"temp_unschedulable", BlockedByTempUnschedulable,
		},
		{
			"inactive flag",
			&Account{Status: StatusActive, Schedulable: false},
			"inactive", BlockedByNotSchedulable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotState, gotBlock := assessSchedulingState(tt.acc, *now)
			if gotState != tt.wantState || gotBlock != tt.wantBlock {
				t.Errorf("assessSchedulingState() = (%q, %q), want (%q, %q)",
					gotState, gotBlock, tt.wantState, tt.wantBlock)
			}
		})
	}
}

func mustParseTime(t *testing.T, s string) *time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("time.Parse(%q): %v", s, err)
	}
	return &parsed
}
