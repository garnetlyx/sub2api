package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestKiroOAuthServiceBuildAccountCredentialsKeepsTokensOnly(t *testing.T) {
	svc := NewKiroOAuthService(nil, nil)
	creds := svc.BuildAccountCredentials(&KiroImportResult{
		AccessToken:  "access",
		RefreshToken: "refresh",
	})

	require.Equal(t, "access", creds["access_token"])
	require.Equal(t, "refresh", creds["refresh_token"])
	_, hasModelMapping := creds["model_mapping"]
	require.False(t, hasModelMapping)
}

func TestKiroOAuthServiceBuildAccountExtraKeepsIdentityMetadataOnly(t *testing.T) {
	svc := NewKiroOAuthService(nil, nil)
	extra := svc.BuildAccountExtra(&KiroImportResult{
		Region:   "us-east-1",
		AuthType: "device_code",
	})

	require.Equal(t, "us-east-1", extra["region"])
	require.Equal(t, "device_code", extra["auth_type"])
	require.Equal(t, "https://view.awsapps.com/start", extra["oidc_issuer"])
	_, hasAvailableModels := extra["available_models"]
	require.False(t, hasAvailableModels)
}
