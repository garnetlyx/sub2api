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

func TestPublicModelsExplicitOnlyAccessorCoercion(t *testing.T) {
	tests := []struct {
		name string
		extra map[string]any
		want bool
	}{
		{"nil extra", nil, false},
		{"missing key", map[string]any{}, false},
		{"bool true", map[string]any{"public_models_explicit_only": true}, true},
		{"bool false", map[string]any{"public_models_explicit_only": false}, false},
		{"string true", map[string]any{"public_models_explicit_only": "true"}, true},
		{"string false", map[string]any{"public_models_explicit_only": "false"}, false},
		{"string 1", map[string]any{"public_models_explicit_only": "1"}, true},
		{"string 0", map[string]any{"public_models_explicit_only": "0"}, false},
		{"int 1", map[string]any{"public_models_explicit_only": 1}, true},
		{"int 0", map[string]any{"public_models_explicit_only": 0}, false},
		{"float64 1", map[string]any{"public_models_explicit_only": float64(1)}, true},
		{"float64 0", map[string]any{"public_models_explicit_only": float64(0)}, false},
		{"json.Number 1", map[string]any{"public_models_explicit_only": json.Number("1")}, true},
		{"json.Number 0", map[string]any{"public_models_explicit_only": json.Number("0")}, false},
		{"garbage string", map[string]any{"public_models_explicit_only": "banana"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			acc := &Account{Extra: tc.extra}
			require.Equal(t, tc.want, acc.PublicModelsExplicitOnly())
		})
	}
}

func TestPublicizeLiveModelCatalogExplicitOnly(t *testing.T) {
	rawModels := []string{
		"deepseek-v4-flash",
		"deepseek-v4-pro",
		"glm-5.2",
		"qwen3.5-9b",
	}

	tests := []struct {
		name        string
		account     *Account
		raw         []string
		wantModels  []string
		forbidden   []string
		wantMapping map[string]string
	}{
		{
			name:       "flag_absent_preserves_union",
			account:    &Account{},
			raw:        []string{"raw-a", "raw-b"},
			wantModels: []string{"raw-a", "raw-b"},
		},
		{
			name: "flag_false_preserves_union",
			account: &Account{Extra: map[string]any{
				"public_models_explicit_only": false,
				"upstream_models":             map[string]any{"public-x": "raw-a"},
			}},
			raw:        []string{"raw-a", "raw-b"},
			wantModels: []string{"public-x", "raw-a", "raw-b"},
			wantMapping: map[string]string{
				"public-x": "raw-a",
				"raw-a":    "raw-a",
				"raw-b":    "raw-b",
			},
		},
		{
			name: "flag_true_hides_raw_ids_exposes_explicit",
			account: &Account{Extra: map[string]any{
				"public_models_explicit_only": true,
				"upstream_models": map[string]any{
					"Deepseek-V4-Flash-q2-imatrix": "deepseek-v4-flash",
					"glm-5.2-q2":                   "glm-5.2",
				},
			}},
			raw:        rawModels,
			wantModels: []string{"Deepseek-V4-Flash-q2-imatrix", "glm-5.2-q2"},
			forbidden:  []string{"deepseek-v4-flash", "deepseek-v4-pro", "glm-5.2", "qwen3.5-9b"},
			wantMapping: map[string]string{
				"Deepseek-V4-Flash-q2-imatrix": "deepseek-v4-flash",
				"glm-5.2-q2":                   "glm-5.2",
			},
		},
		{
			name: "flag_true_explicit_fallback_when_upstream_not_in_raw",
			account: &Account{Extra: map[string]any{
				"public_models_explicit_only": true,
				"upstream_models": map[string]any{
					"public-not-listed": "some-remote-model",
				},
			}},
			raw:        rawModels,
			wantModels: []string{"public-not-listed"},
			forbidden:  append([]string{}, rawModels...),
			wantMapping: map[string]string{
				"public-not-listed": "some-remote-model",
			},
		},
		{
			name: "flag_true_empty_explicit_fail_closed",
			account: &Account{Extra: map[string]any{
				"public_models_explicit_only": true,
			}},
			raw:         rawModels,
			wantModels:  []string{},
			forbidden:   rawModels,
			wantMapping: map[string]string{},
		},
		{
			name: "flag_true_model_mapping_explicit_counts_too",
			account: &Account{
				Extra: map[string]any{
					"public_models_explicit_only": true,
				},
				Credentials: map[string]any{
					"model_mapping": map[string]any{
						"Qwen3.5-9B-MLX-4bit": "qwen3.5-9b",
					},
				},
			},
			raw:        rawModels,
			wantModels: []string{"Qwen3.5-9B-MLX-4bit"},
			forbidden:  []string{"deepseek-v4-flash", "deepseek-v4-pro", "glm-5.2", "qwen3.5-9b"},
			wantMapping: map[string]string{
				"Qwen3.5-9B-MLX-4bit": "qwen3.5-9b",
			},
		},
		{
			name: "flag_true_string_coercion",
			account: &Account{Extra: map[string]any{
				"public_models_explicit_only": "true",
				"upstream_models": map[string]any{
					"explicit-only": "deepseek-v4-flash",
				},
			}},
			raw:        rawModels,
			wantModels: []string{"explicit-only"},
			forbidden:  rawModels,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			catalog := publicizeLiveModelCatalog(tc.account, tc.raw)
			require.ElementsMatch(t, tc.wantModels, catalog.Models)
			for _, bad := range tc.forbidden {
				require.NotContains(t, catalog.Models, bad, "raw ID %q must not be public under explicit-only", bad)
			}
			for publicModel, upstreamModel := range tc.wantMapping {
				require.Equal(t, upstreamModel, catalog.UpstreamModels[publicModel],
					"upstream mapping for %q", publicModel)
			}
		})
	}
}
