package kiro

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestListModelsUsesKiroCLIListAvailableModelsRequest(t *testing.T) {
	const profileArn = "arn:aws:codewhisperer:us-east-1:account:profile/example"

	var seenRegion string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/", r.URL.Path)
		require.Equal(t, "KIRO_CLI", r.URL.Query().Get("origin"))
		require.Equal(t, profileArn, r.URL.Query().Get("profileArn"))

		require.Equal(t, "application/x-amz-json-1.0", r.Header.Get("Content-Type"))
		require.Equal(t, "Bearer access-token", r.Header.Get("Authorization"))
		require.Equal(t, "AmazonCodeWhispererService.ListAvailableModels", r.Header.Get("X-Amz-Target"))
		require.Equal(t, "false", r.Header.Get("X-Amzn-Codewhisperer-Optout"))
		require.Equal(t, "attempt=1; max=3", r.Header.Get("Amz-Sdk-Request"))
		require.NotEmpty(t, r.Header.Get("Amz-Sdk-Invocation-Id"))
		require.Equal(t, "*/*", r.Header.Get("Accept"))
		require.Contains(t, r.Header.Get("User-Agent"), "app/AmazonQ-For-CLI")
		require.Contains(t, r.Header.Get("X-Amz-User-Agent"), "api/codewhispererruntime")

		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		var payload map[string]string
		require.NoError(t, json.Unmarshal(body, &payload))
		require.Equal(t, "KIRO_CLI", payload["origin"])
		require.Equal(t, profileArn, payload["profileArn"])

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[{"modelId":"claude-opus-4.6","displayName":"Claude Opus 4.6","providerName":"anthropic"}]}`))
	}))
	defer server.Close()

	oldEndpoint := qAPIEndpoint
	qAPIEndpoint = func(region string) string {
		seenRegion = region
		return server.URL
	}
	t.Cleanup(func() {
		qAPIEndpoint = oldEndpoint
	})

	models, err := ListModels(context.Background(), server.Client(), "us-east-1", " access-token ", profileArn)
	require.NoError(t, err)
	require.Equal(t, "us-east-1", seenRegion)
	require.Len(t, models, 1)
	require.Equal(t, "claude-opus-4.6", models[0].ModelID)
	require.Equal(t, "Claude Opus 4.6", models[0].DisplayName)
	require.Equal(t, "anthropic", models[0].ProviderName)
}

func TestListModelsOmitsProfileArnWhenUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "KIRO_CLI", r.URL.Query().Get("origin"))
		require.Empty(t, r.URL.Query().Get("profileArn"))

		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		var payload map[string]string
		require.NoError(t, json.Unmarshal(body, &payload))
		require.Equal(t, "KIRO_CLI", payload["origin"])
		_, hasProfileArn := payload["profileArn"]
		require.False(t, hasProfileArn)

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[{"modelId":"auto"}]}`))
	}))
	defer server.Close()

	oldEndpoint := qAPIEndpoint
	qAPIEndpoint = func(region string) string {
		return server.URL
	}
	t.Cleanup(func() {
		qAPIEndpoint = oldEndpoint
	})

	models, err := ListModels(context.Background(), server.Client(), "us-east-1", "access-token", " ")
	require.NoError(t, err)
	require.Len(t, models, 1)
	require.Equal(t, "auto", models[0].ModelID)
}
