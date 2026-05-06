//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	gocache "github.com/patrickmn/go-cache"
	"github.com/stretchr/testify/require"
)

func TestGatewayService_AntigravityNoLiveSourcePassthrough(t *testing.T) {
	svc := &GatewayService{}
	account := &Account{Platform: PlatformAntigravity, Credentials: map[string]any{}}

	require.True(t, svc.isModelSupportedByAccount(account, "claude-sonnet-4-5"))
	require.True(t, svc.isModelSupportedByAccount(account, "gemini-3-flash"))
	require.True(t, svc.isModelSupportedByAccount(account, "gpt-4"))
	require.True(t, svc.isModelSupportedByAccount(account, ""))
}

func TestGatewayService_AntigravityLiveSourceControlsSupport(t *testing.T) {
	account := Account{
		ID:       9001,
		Platform: PlatformAntigravity,
		Type:     AccountTypeOAuth,
	}
	svc := &GatewayService{
		modelsListCache:    gocache.New(time.Minute, time.Minute),
		modelsListCacheTTL: time.Minute,
	}
	svc.modelsListCache.Set(liveModelSourceCacheKey(account), LiveModelSource{
		Account:    &account,
		Endpoint:   "antigravity",
		Capability: "messages",
		Models:     []string{"claude-sonnet-4.6", "gemini-3.1-pro-high"},
		UpstreamModels: map[string]string{
			"claude-sonnet-4.6":   "claude-sonnet-4-6",
			"gemini-3.1-pro-high": "gemini-3-1-pro-high",
		},
	}, time.Minute)

	require.True(t, svc.isModelSupportedByAccountWithContext(context.Background(), &account, "claude-sonnet-4-6"))
	require.True(t, svc.isModelSupportedByAccountWithContext(context.Background(), &account, "gemini-3.1-pro-high"))
	require.False(t, svc.isModelSupportedByAccountWithContext(context.Background(), &account, "claude-opus-4-7"))

	upstream, source := svc.ResolveUpstreamModelForAccount(context.Background(), &account, "claude-sonnet-4.6")
	require.Equal(t, "claude-sonnet-4-6", upstream)
	require.Equal(t, "live", source)
}
