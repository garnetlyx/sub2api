//go:build unit

package service

import (
	"strings"
	"testing"
)

func TestMatchWildcard(t *testing.T) {
	tests := []struct {
		name     string
		pattern  string
		str      string
		expected bool
	}{
		// 精确匹配
		{"exact match", "claude-sonnet-4-5", "claude-sonnet-4-5", true},
		{"exact mismatch", "claude-sonnet-4-5", "claude-opus-4-5", false},

		// 通配符匹配
		{"wildcard prefix match", "claude-*", "claude-sonnet-4-5", true},
		{"wildcard prefix match 2", "claude-*", "claude-opus-4-5-thinking", true},
		{"wildcard prefix mismatch", "claude-*", "gemini-3-flash", false},
		{"wildcard partial match", "gemini-3*", "gemini-3-flash", true},
		{"wildcard partial match 2", "gemini-3*", "gemini-3-pro-image", true},
		{"wildcard partial mismatch", "gemini-3*", "gemini-2.5-flash", false},

		// 边界情况
		{"empty pattern exact", "", "", true},
		{"empty pattern mismatch", "", "claude", false},
		{"single star", "*", "anything", true},
		{"star at end only", "abc*", "abcdef", true},
		{"star at end empty suffix", "abc*", "abc", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := matchWildcard(tt.pattern, tt.str)
			if result != tt.expected {
				t.Errorf("matchWildcard(%q, %q) = %v, want %v", tt.pattern, tt.str, result, tt.expected)
			}
		})
	}
}

func TestMatchWildcardMappingResult(t *testing.T) {
	tests := []struct {
		name           string
		mapping        map[string]string
		requestedModel string
		expected       string
		matched        bool
	}{
		// 精确匹配优先于通配符
		{
			name: "exact match takes precedence",
			mapping: map[string]string{
				"claude-sonnet-4-5": "claude-sonnet-4-5-exact",
				"claude-*":          "claude-default",
			},
			requestedModel: "claude-sonnet-4-5",
			expected:       "claude-sonnet-4-5-exact",
			matched:        true,
		},

		// 最长通配符优先
		{
			name: "longer wildcard takes precedence",
			mapping: map[string]string{
				"claude-*":         "claude-default",
				"claude-sonnet-*":  "claude-sonnet-default",
				"claude-sonnet-4*": "claude-sonnet-4-series",
			},
			requestedModel: "claude-sonnet-4-5",
			expected:       "claude-sonnet-4-series",
			matched:        true,
		},

		// 单个通配符
		{
			name: "single wildcard",
			mapping: map[string]string{
				"claude-*": "claude-mapped",
			},
			requestedModel: "claude-opus-4-5",
			expected:       "claude-mapped",
			matched:        true,
		},

		// 无匹配返回原始模型
		{
			name: "no match returns original",
			mapping: map[string]string{
				"claude-*": "claude-mapped",
			},
			requestedModel: "gemini-3-flash",
			expected:       "gemini-3-flash",
			matched:        false,
		},

		// 空映射返回原始模型
		{
			name:           "empty mapping returns original",
			mapping:        map[string]string{},
			requestedModel: "claude-sonnet-4-5",
			expected:       "claude-sonnet-4-5",
			matched:        false,
		},

		// Gemini 模型映射
		{
			name: "gemini wildcard mapping",
			mapping: map[string]string{
				"gemini-3*":   "gemini-3-pro-high",
				"gemini-2.5*": "gemini-2.5-flash",
			},
			requestedModel: "gemini-3-flash-preview",
			expected:       "gemini-3-pro-high",
			matched:        true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, matched := matchWildcardMappingResult(tt.mapping, tt.requestedModel)
			if result != tt.expected || matched != tt.matched {
				t.Errorf("matchWildcardMappingResult(%v, %q) = (%q, %v), want (%q, %v)", tt.mapping, tt.requestedModel, result, matched, tt.expected, tt.matched)
			}
		})
	}
}

func TestAccountIsModelSupported(t *testing.T) {
	tests := []struct {
		name           string
		platform       string
		credentials    map[string]any
		requestedModel string
		extra          map[string]any
	}{
		{
			name:           "no mapping allows all",
			credentials:    nil,
			requestedModel: "any-model",
		},
		{
			name: "explicit mapping does not restrict passthrough",
			credentials: map[string]any{
				"model_mapping": map[string]any{
					"claude-sonnet-4-5": "target-model",
				},
			},
			requestedModel: "claude-opus-4-5",
		},
		{
			name: "wildcard mapping does not restrict passthrough",
			credentials: map[string]any{
				"model_mapping": map[string]any{
					"claude-*": "claude-sonnet-4-5",
				},
			},
			requestedModel: "gemini-3-flash",
		},
		{
			name:     "platform-specific aliases remain passthrough",
			platform: PlatformGemini,
			credentials: map[string]any{
				"model_mapping": map[string]any{
					"gemini-3.1-pro-preview": "gemini-3.1-pro-preview",
				},
			},
			requestedModel: "gemini-3.1-pro-preview-customtools",
		},
		{
			name:     "claude style aliases remain passthrough",
			platform: PlatformKiro,
			credentials: map[string]any{
				"model_mapping": map[string]any{
					"claude-sonnet-4-6": "claude-sonnet-4-6",
				},
			},
			requestedModel: "claude-sonnet-4.5",
		},
		{
			name:           "legacy available models list no longer restricts selection",
			credentials:    map[string]any{},
			extra:          map[string]any{"available_models": []any{"deepseek-v4-pro"}, "unsupported_models": []any{"gpt-5.4"}},
			requestedModel: "deepseek-v4-pro",
		},
		{
			name:           "legacy model lists are ignored for unmatched model too",
			credentials:    map[string]any{},
			extra:          map[string]any{"available_models": []any{"deepseek-v4-pro"}},
			requestedModel: "deepseek-v4-flash",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			account := &Account{
				Platform:    tt.platform,
				Credentials: tt.credentials,
				Extra:       tt.extra,
			}
			result := account.IsModelSupported(tt.requestedModel)
			if !result {
				t.Errorf("IsModelSupported(%q) = false, want true", tt.requestedModel)
			}
		})
	}
}

func TestAccountGetMappedModel(t *testing.T) {
	tests := []struct {
		name           string
		platform       string
		credentials    map[string]any
		extra          map[string]any
		requestedModel string
		expected       string
	}{
		// 无映射 = 返回原始模型
		{
			name:           "no mapping returns original",
			credentials:    nil,
			requestedModel: "claude-sonnet-4-5",
			expected:       "claude-sonnet-4-5",
		},
		{
			name:           "no mapping preserves gemini customtools model",
			platform:       PlatformGemini,
			credentials:    nil,
			requestedModel: "gemini-3.1-pro-preview-customtools",
			expected:       "gemini-3.1-pro-preview-customtools",
		},
		{
			name:           "available upstream model maps native provider id",
			credentials:    map[string]any{},
			extra:          map[string]any{"upstream_models": map[string]any{"deepseek-v4-pro": "deepseek-v4-pro-actual"}},
			requestedModel: "deepseek-v4-pro",
			expected:       "deepseek-v4-pro-actual",
		},

		// 精确匹配
		{
			name: "exact match",
			credentials: map[string]any{
				"model_mapping": map[string]any{
					"claude-sonnet-4-5": "target-model",
				},
			},
			requestedModel: "claude-sonnet-4-5",
			expected:       "target-model",
		},

		// 通配符匹配（最长优先）
		{
			name: "wildcard longest match",
			credentials: map[string]any{
				"model_mapping": map[string]any{
					"claude-*":        "claude-default",
					"claude-sonnet-*": "claude-sonnet-mapped",
				},
			},
			requestedModel: "claude-sonnet-4-5",
			expected:       "claude-sonnet-mapped",
		},

		// 无匹配返回原始模型
		{
			name:     "gemini customtools alias resolves through normalized mapping",
			platform: PlatformGemini,
			credentials: map[string]any{
				"model_mapping": map[string]any{
					"gemini-3.1-pro-preview": "gemini-3.1-pro-preview",
				},
			},
			requestedModel: "gemini-3.1-pro-preview-customtools",
			expected:       "gemini-3.1-pro-preview",
		},
		{
			name:     "gemini customtools exact mapping wins over normalized fallback",
			platform: PlatformGemini,
			credentials: map[string]any{
				"model_mapping": map[string]any{
					"gemini-3.1-pro-preview":             "gemini-3.1-pro-preview",
					"gemini-3.1-pro-preview-customtools": "gemini-3.1-pro-preview-customtools",
				},
			},
			requestedModel: "gemini-3.1-pro-preview-customtools",
			expected:       "gemini-3.1-pro-preview-customtools",
		},
		{
			name: "no match returns original",
			credentials: map[string]any{
				"model_mapping": map[string]any{
					"gemini-*": "gemini-mapped",
				},
			},
			requestedModel: "claude-sonnet-4-5",
			expected:       "claude-sonnet-4-5",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			account := &Account{
				Platform:    tt.platform,
				Credentials: tt.credentials,
				Extra:       tt.extra,
			}
			result := account.GetMappedModel(tt.requestedModel)
			if result != tt.expected {
				t.Errorf("GetMappedModel(%q) = %q, want %q", tt.requestedModel, result, tt.expected)
			}
		})
	}
}

func TestAccountResolveMappedModel(t *testing.T) {
	tests := []struct {
		name           string
		platform       string
		credentials    map[string]any
		requestedModel string
		expectedModel  string
		expectedMatch  bool
	}{
		{
			name:           "no mapping reports unmatched",
			credentials:    nil,
			requestedModel: "gpt-5.4",
			expectedModel:  "gpt-5.4",
			expectedMatch:  false,
		},
		{
			name: "exact passthrough mapping still counts as matched",
			credentials: map[string]any{
				"model_mapping": map[string]any{
					"gpt-5.4": "gpt-5.4",
				},
			},
			requestedModel: "gpt-5.4",
			expectedModel:  "gpt-5.4",
			expectedMatch:  true,
		},
		{
			name: "wildcard passthrough mapping still counts as matched",
			credentials: map[string]any{
				"model_mapping": map[string]any{
					"gpt-*": "gpt-5.4",
				},
			},
			requestedModel: "gpt-5.4",
			expectedModel:  "gpt-5.4",
			expectedMatch:  true,
		},
		{
			name:     "gemini customtools alias reports normalized match",
			platform: PlatformGemini,
			credentials: map[string]any{
				"model_mapping": map[string]any{
					"gemini-3.1-pro-preview": "gemini-3.1-pro-preview",
				},
			},
			requestedModel: "gemini-3.1-pro-preview-customtools",
			expectedModel:  "gemini-3.1-pro-preview",
			expectedMatch:  true,
		},
		{
			name:     "gemini customtools exact mapping reports exact match",
			platform: PlatformGemini,
			credentials: map[string]any{
				"model_mapping": map[string]any{
					"gemini-3.1-pro-preview":             "gemini-3.1-pro-preview",
					"gemini-3.1-pro-preview-customtools": "gemini-3.1-pro-preview-customtools",
				},
			},
			requestedModel: "gemini-3.1-pro-preview-customtools",
			expectedModel:  "gemini-3.1-pro-preview-customtools",
			expectedMatch:  true,
		},
		{
			name:     "anthropic platform accepts openai compatibility alias",
			platform: PlatformAnthropic,
			credentials: map[string]any{
				"model_mapping": map[string]any{
					"anthropic/GLM-5.1": "anthropic/GLM-5.1",
				},
			},
			requestedModel: "openai/GLM-5.1",
			expectedModel:  "anthropic/GLM-5.1",
			expectedMatch:  true,
		},
		{
			name:     "openai platform accepts anthropic compatibility alias",
			platform: PlatformOpenAI,
			credentials: map[string]any{
				"model_mapping": map[string]any{
					"openai/GLM-5.1": "openai/GLM-5.1",
				},
			},
			requestedModel: "anthropic/GLM-5.1",
			expectedModel:  "openai/GLM-5.1",
			expectedMatch:  true,
		},
		{
			name:     "claude dotted alias resolves to kiro hyphen upstream id",
			platform: PlatformKiro,
			credentials: map[string]any{
				"model_mapping": map[string]any{
					"claude-sonnet-4-6": "claude-sonnet-4-6",
				},
			},
			requestedModel: "claude-sonnet-4.6",
			expectedModel:  "claude-sonnet-4-6",
			expectedMatch:  true,
		},
		{
			name:     "claude thinking alias resolves to hyphen upstream id",
			platform: PlatformAnthropic,
			credentials: map[string]any{
				"model_mapping": map[string]any{
					"claude-opus-4-6-thinking": "claude-opus-4-6-thinking",
				},
			},
			requestedModel: "claude-opus-4.6-thinking",
			expectedModel:  "claude-opus-4-6-thinking",
			expectedMatch:  true,
		},
		{
			name: "hidden proxy alias resolves to bare mapping",
			credentials: map[string]any{
				"model_mapping": map[string]any{
					"ark-code-latest": "ark-code-latest",
				},
			},
			requestedModel: "ark-code-latest-volcengine",
			expectedModel:  "ark-code-latest",
			expectedMatch:  true,
		},
		{
			name:     "dated claude ids are not style-normalized",
			platform: PlatformKiro,
			credentials: map[string]any{
				"model_mapping": map[string]any{
					"claude-haiku-4-5-20251001": "claude-haiku-4-5-20251001",
				},
			},
			requestedModel: "claude-haiku-4.5",
			expectedModel:  "claude-haiku-4.5",
			expectedMatch:  false,
		},
		{
			name: "missing mapping reports unmatched",
			credentials: map[string]any{
				"model_mapping": map[string]any{
					"gpt-5.2": "gpt-5.2",
				},
			},
			requestedModel: "gpt-5.4",
			expectedModel:  "gpt-5.4",
			expectedMatch:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			account := &Account{
				Platform:    tt.platform,
				Credentials: tt.credentials,
			}
			mappedModel, matched := account.ResolveMappedModel(tt.requestedModel)
			if mappedModel != tt.expectedModel || matched != tt.expectedMatch {
				t.Fatalf("ResolveMappedModel(%q) = (%q, %v), want (%q, %v)", tt.requestedModel, mappedModel, matched, tt.expectedModel, tt.expectedMatch)
			}
		})
	}
}

func TestDotHyphenAlternate(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"gpt-5.5", "gpt-5-5"},
		{"gpt-5-5", "gpt-5.5"},
		{"gpt-5-5-pro", "gpt-5.5-pro"},
		{"gpt-5.5-pro", "gpt-5-5-pro"},
		{"gpt-5-5-thinking", "gpt-5.5-thinking"},
		{"gpt-4-5", "gpt-4.5"},
		{"gpt-4.5", "gpt-4-5"},
		{"gpt-5-2-instant", "gpt-5.2-instant"},
		{"gpt-5-4-pro", "gpt-5.4-pro"},
		{"gpt-5-3-mini", "gpt-5.3-mini"},
		{"minimax-m2.7", "minimax-m2-7"},
		{"minimax-m2-7", "minimax-m2.7"},
		{"glm-5.1", "glm-5-1"},
		{"glm-5-1", "glm-5.1"},
		{"deepseek-v3.2", "deepseek-v3-2"},
		{"deepseek-v3-2", "deepseek-v3.2"},
		{"claude-sonnet-4.6", "claude-sonnet-4-6"},
		{"claude-sonnet-4-6", "claude-sonnet-4.6"},
		// No version separator → empty
		{"gpt-5-mini", ""},
		{"agent-mode", ""},
		{"o3", ""},
		// Precise dated upstream IDs are not style aliases.
		{"claude-3-5-sonnet-20241022", ""},
	}
	for _, tt := range tests {
		got := publicModelStyleAlternate(tt.input)
		if got != tt.want {
			t.Errorf("publicModelStyleAlternate(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestModelListContainsRequestedModel_DotHyphenEquivalence(t *testing.T) {
	tests := []struct {
		modelIDs       []string
		requestedModel string
		want           bool
	}{
		// Dot requested, hyphen in list
		{[]string{"gpt-5-5"}, "gpt-5.5", true},
		{[]string{"gpt-5-5-pro"}, "gpt-5.5-pro", true},
		{[]string{"gpt-4-5"}, "gpt-4.5", true},
		{[]string{"minimax-m2-7"}, "minimax-m2.7", true},
		{[]string{"glm-5-1"}, "glm-5.1", true},
		{[]string{"deepseek-v3-2"}, "deepseek-v3.2", true},
		{[]string{"claude-sonnet-4-6"}, "claude-sonnet-4.6", true},
		// Hyphen requested, dot in list
		{[]string{"gpt-5.5"}, "gpt-5-5", true},
		{[]string{"minimax-m2.7"}, "minimax-m2-7", true},
		// No match
		{[]string{"gpt-5-4-pro"}, "gpt-5.5", false},
		{[]string{"gpt-5-mini"}, "gpt-5.5", false},
		{[]string{"claude-3-5-sonnet-20241022"}, "claude-3.5-sonnet-20241022", false},
		// Mixed list with suffixes
		{[]string{"gpt-5-5", "gpt-5-5-pro", "gpt-5-5-thinking"}, "gpt-5.5-pro", true},
	}
	for _, tt := range tests {
		got := modelListContainsRequestedModel(tt.modelIDs, tt.requestedModel)
		if got != tt.want {
			t.Errorf("modelListContainsRequestedModel(%v, %q) = %v, want %v",
				tt.modelIDs, tt.requestedModel, got, tt.want)
		}
	}
}

func TestModelListContainsRequestedModel_CodexWMStrip(t *testing.T) {
	// codex/models manifest lists bare slugs (gpt-5.6-luna) but clients
	// request the -wm conversation variant. Matching must succeed so the
	// account is not filtered out of scheduling.
	tests := []struct {
		modelIDs       []string
		requestedModel string
		want           bool
	}{
		{[]string{"gpt-5.6-luna"}, "gpt-5.6-luna-wm", true},
		{[]string{"gpt-5.6-terra"}, "gpt-5.6-terra-wm", true},
		{[]string{"gpt-5.5"}, "gpt-5.5-wm", true},
		{[]string{"gpt-5.6-sol", "gpt-5.6-sol-wm"}, "gpt-5.6-sol-wm", true},
		{[]string{"gpt-5.6-sol"}, "gpt-5.6-sol", true},
		// Non-wm models still match normally
		{[]string{"gpt-5.5"}, "gpt-5.5", true},
		// Genuinely absent
		{[]string{"gpt-5.4"}, "gpt-5.6-luna-wm", false},
	}
	for _, tt := range tests {
		got := modelListContainsRequestedModel(tt.modelIDs, tt.requestedModel)
		if got != tt.want {
			t.Errorf("modelListContainsRequestedModel(%v, %q) = %v, want %v",
				tt.modelIDs, tt.requestedModel, got, tt.want)
		}
	}
}

func TestRequestedModelLookupCandidates_DotHyphen(t *testing.T) {
	candidates := requestedModelLookupCandidates("", "gpt-5.5")
	has := func(target string) bool {
		for _, c := range candidates {
			if strings.EqualFold(c, target) {
				return true
			}
		}
		return false
	}
	if !has("gpt-5.5") {
		t.Error("expected gpt-5.5 in candidates")
	}
	if !has("gpt-5-5") {
		t.Error("expected gpt-5-5 (dot-hyphen alternate) in candidates")
	}
}

func TestCanonicalizePublicModel_HiddenProxyAliases(t *testing.T) {
	tests := map[string]string{
		"ark-code-latest-volcengine":           "ark-code-latest",
		"deepseek-v3.2-volcengine":             "deepseek-v3.2",
		"doubao-seed-2.0-pro-volcengine":       "doubao-seed-2.0-pro",
		"glm-5-turbo-zhipu":                    "glm-5-turbo",
		"glm-5.1-zhipu":                        "glm-5.1",
		"kimi-k2.5-volcengine":                 "kimi-k2.5",
		"minimax-m2.7-minimax":                 "minimax-m2.7",
		"openai/glm-5.1-zhipu":                 "openai/glm-5.1",
		"gpt-5-5":                              "gpt-5.5",
		"openai/gpt-5-5":                       "openai/gpt-5.5",
		"minimax-m2-7":                         "minimax-m2.7",
		"glm-5-1":                              "glm-5.1",
		"deepseek-v3-2":                        "deepseek-v3.2",
		"claude-sonnet-4-6":                    "claude-sonnet-4.6",
		"claude-opus-4-7-thinking":             "claude-opus-4.7-thinking",
		"claude-3-5-sonnet-20241022":           "claude-3-5-sonnet-20241022",
		"anthropic/claude-3-5-sonnet-20241022": "anthropic/claude-3-5-sonnet-20241022",
	}

	for input, expected := range tests {
		if got := CanonicalizePublicModel(input); got != expected {
			t.Fatalf("CanonicalizePublicModel(%q) = %q, want %q", input, got, expected)
		}
	}
}

func TestAccountGetModelMapping_AntigravityUsesOnlyDeclaredMappings(t *testing.T) {
	account := &Account{
		Platform: PlatformAntigravity,
		Credentials: map[string]any{
			"model_mapping": map[string]any{
				"gemini-3-pro-high": "gemini-3.1-pro-high",
			},
		},
	}

	mapping := account.GetModelMapping()
	if len(mapping) != 1 || mapping["gemini-3-pro-high"] != "gemini-3.1-pro-high" {
		t.Fatalf("expected only the declared dynamic mapping, got: %#v", mapping)
	}
	for _, undeclared := range []string{"gemini-3-flash", "gemini-3.1-pro-high", "gemini-3.1-pro-low"} {
		if _, exists := mapping[undeclared]; exists {
			t.Fatalf("did not expect hardcoded passthrough for undeclared model %q", undeclared)
		}
	}
}

func TestAccountGetModelMapping_AntigravityRespectsWildcardOverride(t *testing.T) {
	account := &Account{
		Platform: PlatformAntigravity,
		Credentials: map[string]any{
			"model_mapping": map[string]any{
				"gemini-3*": "gemini-3.1-pro-high",
			},
		},
	}

	mapping := account.GetModelMapping()
	if _, exists := mapping["gemini-3-flash"]; exists {
		t.Fatalf("did not expect explicit gemini-3-flash passthrough when wildcard already exists")
	}
	if _, exists := mapping["gemini-3.1-pro-high"]; exists {
		t.Fatalf("did not expect explicit gemini-3.1-pro-high passthrough when wildcard already exists")
	}
	if _, exists := mapping["gemini-3.1-pro-low"]; exists {
		t.Fatalf("did not expect explicit gemini-3.1-pro-low passthrough when wildcard already exists")
	}
	if mapped := account.GetMappedModel("gemini-3-flash"); mapped != "gemini-3.1-pro-high" {
		t.Fatalf("expected wildcard mapping to stay effective, got: %q", mapped)
	}
}

func TestAccountGetModelMapping_CacheInvalidatesOnCredentialsReplace(t *testing.T) {
	account := &Account{
		Credentials: map[string]any{
			"model_mapping": map[string]any{
				"claude-3-5-sonnet": "upstream-a",
			},
		},
	}

	first := account.GetModelMapping()
	if first["claude-3-5-sonnet"] != "upstream-a" {
		t.Fatalf("unexpected first mapping: %v", first)
	}

	account.Credentials = map[string]any{
		"model_mapping": map[string]any{
			"claude-3-5-sonnet": "upstream-b",
		},
	}
	second := account.GetModelMapping()
	if second["claude-3-5-sonnet"] != "upstream-b" {
		t.Fatalf("expected cache invalidated after credentials replace, got: %v", second)
	}
}

func TestAccountGetModelMapping_CacheInvalidatesOnMappingLenChange(t *testing.T) {
	rawMapping := map[string]any{
		"claude-sonnet": "sonnet-a",
	}
	account := &Account{
		Credentials: map[string]any{
			"model_mapping": rawMapping,
		},
	}

	first := account.GetModelMapping()
	if len(first) != 1 {
		t.Fatalf("unexpected first mapping length: %d", len(first))
	}

	rawMapping["claude-opus"] = "opus-b"
	second := account.GetModelMapping()
	if second["claude-opus"] != "opus-b" {
		t.Fatalf("expected cache invalidated after mapping len change, got: %v", second)
	}
}

func TestAccountGetModelMapping_CacheInvalidatesOnInPlaceValueChange(t *testing.T) {
	rawMapping := map[string]any{
		"claude-sonnet": "sonnet-a",
	}
	account := &Account{
		Credentials: map[string]any{
			"model_mapping": rawMapping,
		},
	}

	first := account.GetModelMapping()
	if first["claude-sonnet"] != "sonnet-a" {
		t.Fatalf("unexpected first mapping: %v", first)
	}

	rawMapping["claude-sonnet"] = "sonnet-b"
	second := account.GetModelMapping()
	if second["claude-sonnet"] != "sonnet-b" {
		t.Fatalf("expected cache invalidated after in-place value change, got: %v", second)
	}
}
