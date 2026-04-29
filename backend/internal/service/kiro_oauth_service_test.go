package service

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"github.com/stretchr/testify/require"
)

func TestKiroOAuthServiceBuildAccountCredentialsIncludesModelMapping(t *testing.T) {
	svc := NewKiroOAuthService(nil, nil)
	creds := svc.BuildAccountCredentials(&KiroImportResult{
		AccessToken:  "access",
		RefreshToken: "refresh",
		AvailableModels: []kiro.ModelInfo{
			{ModelID: "claude-sonnet-4-5"},
			{ModelID: "claude-haiku-4.5"},
		},
	})

	mapping, ok := creds["model_mapping"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "claude-sonnet-4-5", mapping["claude-sonnet-4.5"])
	require.Equal(t, "claude-haiku-4.5", mapping["claude-haiku-4.5"])
}

func TestKiroOAuthServiceBuildAccountExtraUsesCanonicalAvailableModels(t *testing.T) {
	svc := NewKiroOAuthService(nil, nil)
	extra := svc.BuildAccountExtra(&KiroImportResult{
		Region:   "us-east-1",
		AuthType: "device_code",
		AvailableModels: []kiro.ModelInfo{
			{ModelID: "claude-sonnet-4-5"},
		},
	})

	require.Equal(t, []string{"claude-sonnet-4.5"}, extra["available_models"])
}
