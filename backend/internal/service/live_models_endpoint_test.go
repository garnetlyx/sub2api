package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

// liveModelsHTTPStub delegates to a httptest.Server for realistic request/response testing.
type liveModelsHTTPStub struct {
	client *http.Client
}

func (s *liveModelsHTTPStub) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return s.client.Do(req)
}

func (s *liveModelsHTTPStub) DoWithTLS(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return s.client.Do(req)
}

func TestLiveOpenAIModelEndpoint(t *testing.T) {
	tests := []struct {
		name string
		base string
		want string
	}{
		{"empty", "", ""},
		{"standard", "https://api.example.com", "https://api.example.com/v1/models"},
		{"trailing v1", "https://api.example.com/v1", "https://api.example.com/v1/models"},
		{"trailing slash", "https://api.example.com/", "https://api.example.com/v1/models"},
		{"already models", "https://api.example.com/models", "https://api.example.com/models"},
		{"non-standard v3", "https://ark.cn-beijing.volces.com/api/coding/v3", "https://ark.cn-beijing.volces.com/api/coding/v3/v1/models"},
		{"non-standard v4", "https://open.bigmodel.cn/api/paas/v4", "https://open.bigmodel.cn/api/paas/v4/v1/models"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := liveOpenAIModelEndpoint(tc.base)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestLiveOpenAIModelFallbackEndpoint(t *testing.T) {
	tests := []struct {
		name string
		base string
		want string
	}{
		{"empty", "", ""},
		{"standard", "https://api.example.com", "https://api.example.com/models"},
		{"non-standard v3", "https://ark.cn-beijing.volces.com/api/coding/v3", "https://ark.cn-beijing.volces.com/api/coding/v3/models"},
		{"non-standard v4", "https://open.bigmodel.cn/api/paas/v4", "https://open.bigmodel.cn/api/paas/v4/models"},
		{"trailing slash", "https://api.example.com/", "https://api.example.com/models"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := liveOpenAIModelFallbackEndpoint(tc.base)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestLiveAnthropicModelEndpoint(t *testing.T) {
	tests := []struct {
		name string
		base string
		want string
	}{
		{"empty", "", ""},
		{"standard", "https://api.example.com", "https://api.example.com/v1/models"},
		{"trailing v1", "https://api.example.com/v1", "https://api.example.com/v1/models"},
		{"already models", "https://api.example.com/models", "https://api.example.com/models"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := liveAnthropicModelEndpoint(tc.base)
			require.Equal(t, tc.want, got)
		})
	}
}

func makeAccountForOpenAI(platform, baseURL, apiKey string) *Account {
	return &Account{
		Platform: platform,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"base_url": baseURL,
			"api_key":  apiKey,
		},
	}
}

func makeAccountForAnthropic(baseURL, apiKey string) *Account {
	return &Account{
		Platform: PlatformAnthropic,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"base_url": baseURL,
			"api_key":  apiKey,
		},
	}
}

func TestLiveOpenAICompatibleModelsFallback(t *testing.T) {
	modelsJSON, _ := json.Marshal(map[string]any{
		"data": []map[string]string{
			{"id": "doubao-seed-2.0-pro"},
			{"id": "glm-5.1"},
		},
	})

	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if r.URL.Path == "/api/coding/v3/v1/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.URL.Path == "/api/coding/v3/models" {
			w.Header().Set("Content-Type", "application/json")
			w.Write(modelsJSON)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	gs := &GatewayService{
		httpUpstream: &liveModelsHTTPStub{client: server.Client()},
	}
	account := makeAccountForOpenAI(PlatformOpenAI, server.URL+"/api/coding/v3", "test-key")

	models, err := gs.liveOpenAICompatibleModels(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, []string{"doubao-seed-2.0-pro", "glm-5.1"}, models)
	require.Equal(t, 2, callCount)
}

func TestLiveOpenAICompatibleModelsNoFallbackOnSuccess(t *testing.T) {
	modelsJSON, _ := json.Marshal(map[string]any{
		"data": []map[string]string{{"id": "deepseek-v4-flash"}},
	})

	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		w.Write(modelsJSON)
	}))
	defer server.Close()

	gs := &GatewayService{
		httpUpstream: &liveModelsHTTPStub{client: server.Client()},
	}
	account := makeAccountForOpenAI(PlatformOpenAI, server.URL, "test-key")

	models, err := gs.liveOpenAICompatibleModels(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, []string{"deepseek-v4-flash"}, models)
	require.Equal(t, 1, callCount)
}

func TestLiveAnthropicModelsBearerFallback(t *testing.T) {
	modelsJSON, _ := json.Marshal(map[string]any{
		"data": []map[string]string{
			{"id": "doubao-seed-2.0-pro"},
		},
	})

	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if r.Header.Get("x-api-key") != "" && r.Header.Get("Authorization") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Authorization") != "" {
			w.Header().Set("Content-Type", "application/json")
			w.Write(modelsJSON)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	gs := &GatewayService{
		httpUpstream: &liveModelsHTTPStub{client: server.Client()},
	}
	account := makeAccountForAnthropic(server.URL, "test-key")

	models, err := gs.liveAnthropicModels(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, []string{"doubao-seed-2.0-pro"}, models)
	require.Equal(t, 2, callCount)
}

func TestLiveAnthropicModelsNoFallbackOnXAPIKeySuccess(t *testing.T) {
	modelsJSON, _ := json.Marshal(map[string]any{
		"data": []map[string]string{{"id": "minimax-m2.7"}},
	})

	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		w.Write(modelsJSON)
	}))
	defer server.Close()

	gs := &GatewayService{
		httpUpstream: &liveModelsHTTPStub{client: server.Client()},
	}
	account := makeAccountForAnthropic(server.URL, "test-key")

	models, err := gs.liveAnthropicModels(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, []string{"minimax-m2.7"}, models)
	require.Equal(t, 1, callCount)
}

func TestLiveAnthropicCrossProtocolEndpoint(t *testing.T) {
	tests := []struct {
		name string
		base string
		want string
	}{
		{"empty", "", ""},
		{"bare domain", "https://api.anthropic.com", ""},
		{"bare domain trailing slash", "https://api.anthropic.com/", ""},
		{"deepseek anthropic", "https://api.deepseek.com/anthropic", "https://api.deepseek.com/v1/models"},
		{"deepseek anthropic trailing slash", "https://api.deepseek.com/anthropic/", "https://api.deepseek.com/v1/models"},
		{"nested path", "https://api.example.com/v1/anthropic", "https://api.example.com/v1/v1/models"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := liveAnthropicCrossProtocolEndpoint(tc.base)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestLiveAnthropicModelsCrossProtocolFallback(t *testing.T) {
	modelsJSON, _ := json.Marshal(map[string]any{
		"data": []map[string]string{
			{"id": "deepseek-v4-flash"},
			{"id": "deepseek-v4-pro"},
		},
	})

	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if r.URL.Path == "/anthropic/v1/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.URL.Path == "/v1/models" {
			if r.Header.Get("Authorization") == "" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write(modelsJSON)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	gs := &GatewayService{
		httpUpstream: &liveModelsHTTPStub{client: server.Client()},
	}
	account := makeAccountForAnthropic(server.URL+"/anthropic", "test-key")

	models, err := gs.liveAnthropicModels(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, []string{"deepseek-v4-flash", "deepseek-v4-pro"}, models)
	require.Equal(t, 2, callCount)
}

func TestLiveAnthropicModelsCrossProtocolSkippedForBareDomain(t *testing.T) {
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	gs := &GatewayService{
		httpUpstream: &liveModelsHTTPStub{client: server.Client()},
	}
	account := makeAccountForAnthropic(server.URL, "test-key")

	_, err := gs.liveAnthropicModels(context.Background(), account)
	require.Error(t, err)
	require.Equal(t, 1, callCount)
}

func TestPublicizeLiveModelCatalogUnion(t *testing.T) {
	account := &Account{
		Extra: map[string]any{
			"upstream_models": map[string]any{
				"doubao-seed-2.0-pro": "doubao-seed-2.0-pro",
				"ark-code-latest":     "ark-code-latest",
			},
		},
	}
	rawModels := []string{"some-other-model", "unrelated-model"}

	catalog := publicizeLiveModelCatalog(account, rawModels)

	require.Contains(t, catalog.Models, "doubao-seed-2.0-pro")
	require.Contains(t, catalog.Models, "ark-code-latest")
	require.Contains(t, catalog.Models, "some-other-model")
	require.Equal(t, "doubao-seed-2.0-pro", catalog.UpstreamModels["doubao-seed-2.0-pro"])
	require.Equal(t, "ark-code-latest", catalog.UpstreamModels["ark-code-latest"])
}

func TestPublicizeLiveModelCatalogLiveOverridesConfig(t *testing.T) {
	account := &Account{
		Extra: map[string]any{
			"upstream_models": map[string]any{
				"glm-5.1": "glm-5.1",
			},
		},
	}
	rawModels := []string{"glm-5.1"}

	catalog := publicizeLiveModelCatalog(account, rawModels)

	require.Contains(t, catalog.Models, "glm-5.1")
	require.Equal(t, "glm-5.1", catalog.UpstreamModels["glm-5.1"])
}
