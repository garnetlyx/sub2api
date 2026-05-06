//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAntigravityGatewayService_GetMappedModel(t *testing.T) {
	svc := &AntigravityGatewayService{}

	tests := []struct {
		name           string
		requestedModel string
		accountMapping map[string]string
		expected       string
	}{
		{
			name:           "explicit account mapping",
			requestedModel: "claude-3-5-sonnet-20241022",
			accountMapping: map[string]string{"claude-3-5-sonnet-20241022": "custom-model"},
			expected:       "custom-model",
		},
		{
			name:           "explicit account mapping for current model",
			requestedModel: "claude-sonnet-4-5",
			accountMapping: map[string]string{"claude-sonnet-4-5": "my-custom-sonnet"},
			expected:       "my-custom-sonnet",
		},
		{
			name:           "explicit account mapping for custom model",
			requestedModel: "claude-opus-4",
			accountMapping: map[string]string{"claude-opus-4": "my-opus"},
			expected:       "my-opus",
		},
		{
			name:           "no explicit mapping passes through",
			requestedModel: "claude-opus-4-6",
			accountMapping: nil,
			expected:       "claude-opus-4-6",
		},
		{
			name:           "custom model without mapping passes through",
			requestedModel: "claude-unknown",
			accountMapping: nil,
			expected:       "claude-unknown",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			account := &Account{
				Platform: PlatformAntigravity,
			}
			if tt.accountMapping != nil {
				mappingAny := make(map[string]any)
				for k, v := range tt.accountMapping {
					mappingAny[k] = v
				}
				account.Credentials = map[string]any{
					"model_mapping": mappingAny,
				}
			}

			got := svc.getMappedModel(context.Background(), account, tt.requestedModel)
			require.Equal(t, tt.expected, got, "model: %s", tt.requestedModel)
		})
	}
}

func TestAntigravityGatewayService_GetMappedModel_EdgeCases(t *testing.T) {
	svc := &AntigravityGatewayService{}

	tests := []struct {
		name           string
		requestedModel string
		expected       string
	}{
		{"空字符串", "", ""},
		{"non claude/gemini model passes through", "gpt-4", "gpt-4"},
		{"custom local-style model passes through", "llama-3", "llama-3"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			account := &Account{Platform: PlatformAntigravity}
			got := svc.getMappedModel(context.Background(), account, tt.requestedModel)
			require.Equal(t, tt.expected, got)
		})
	}
}

// TestMapAntigravityModel_WildcardTargetEqualsRequest 测试通配符映射目标恰好等于请求模型名的 edge case
// 例如 {"claude-*": "claude-sonnet-4-5"}，请求 "claude-sonnet-4-5" 时应该通过
func TestMapAntigravityModel_WildcardTargetEqualsRequest(t *testing.T) {
	tests := []struct {
		name           string
		modelMapping   map[string]any
		requestedModel string
		expected       string
	}{
		{
			name:           "wildcard target equals request model",
			modelMapping:   map[string]any{"claude-*": "claude-sonnet-4-5"},
			requestedModel: "claude-sonnet-4-5",
			expected:       "claude-sonnet-4-5",
		},
		{
			name:           "wildcard target differs from request model",
			modelMapping:   map[string]any{"claude-*": "claude-sonnet-4-5"},
			requestedModel: "claude-opus-4-6",
			expected:       "claude-sonnet-4-5",
		},
		{
			name:           "wildcard no match",
			modelMapping:   map[string]any{"claude-*": "claude-sonnet-4-5"},
			requestedModel: "gpt-4o",
			expected:       "gpt-4o",
		},
		{
			name:           "explicit passthrough same name",
			modelMapping:   map[string]any{"claude-sonnet-4-5": "claude-sonnet-4-5"},
			requestedModel: "claude-sonnet-4-5",
			expected:       "claude-sonnet-4-5",
		},
		{
			name:           "multiple wildcards target equals one request",
			modelMapping:   map[string]any{"claude-*": "claude-sonnet-4-5", "gemini-*": "gemini-2.5-flash"},
			requestedModel: "gemini-2.5-flash",
			expected:       "gemini-2.5-flash",
		},
		{
			name:           "customtools alias falls back to normalized preview mapping",
			modelMapping:   map[string]any{"gemini-3.1-pro-preview": "gemini-3.1-pro-high"},
			requestedModel: "gemini-3.1-pro-preview-customtools",
			expected:       "gemini-3.1-pro-high",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			account := &Account{
				Platform: PlatformAntigravity,
				Credentials: map[string]any{
					"model_mapping": tt.modelMapping,
				},
			}
			got := mapAntigravityModel(account, tt.requestedModel)
			require.Equal(t, tt.expected, got, "mapAntigravityModel(%q) = %q, want %q", tt.requestedModel, got, tt.expected)
		})
	}
}
