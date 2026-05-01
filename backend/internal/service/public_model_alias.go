package service

import (
	"fmt"
	"regexp"
	"strings"
)

var claudeStyleAliasPattern = regexp.MustCompile(`^(claude-(?:opus|sonnet|haiku))-(\d+)([.-])(\d+)(-.+)?$`)

var hiddenPublicModelAliases = map[string]string{
	"ark-code-latest-volcengine":     "ark-code-latest",
	"deepseek-v3.2-volcengine":       "deepseek-v3.2",
	"doubao-seed-2.0-pro-volcengine": "doubao-seed-2.0-pro",
	"glm-5-turbo-zhipu":              "glm-5-turbo",
	"glm-5.1-zhipu":                  "glm-5.1",
	"kimi-k2.5-volcengine":           "kimi-k2.5",
	"minimax-m2.7-minimax":           "minimax-m2.7",
}

// CanonicalizePublicModel normalizes style-only public model aliases to the
// dotted public form while preserving provider-specific upstream IDs.
func CanonicalizePublicModel(model string) string {
	trimmed := strings.TrimSpace(model)
	if trimmed == "" {
		return ""
	}

	prefix := ""
	name := trimmed
	if idx := strings.Index(trimmed, "/"); idx > 0 && idx < len(trimmed)-1 {
		prefix = trimmed[:idx+1]
		name = trimmed[idx+1:]
	}

	if canonical, ok := hiddenPublicModelAliases[strings.ToLower(name)]; ok {
		return prefix + canonical
	}

	matches := claudeStyleAliasPattern.FindStringSubmatch(name)
	if matches == nil {
		return trimmed
	}

	suffix := matches[5]
	if looksLikeClaudeDateSuffix(suffix) {
		return trimmed
	}

	canonical := fmt.Sprintf("%s-%s.%s%s", matches[1], matches[2], matches[4], suffix)
	return prefix + canonical
}

func looksLikeClaudeDateSuffix(suffix string) bool {
	if suffix == "" {
		return false
	}
	trimmed := strings.TrimPrefix(suffix, "-")
	if len(trimmed) < 8 {
		return false
	}
	datePart := trimmed[:8]
	for _, ch := range datePart {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return len(trimmed) == 8 || trimmed[8] == '-'
}

func publicModelStyleAlternate(model string) string {
	trimmed := strings.TrimSpace(model)
	if trimmed == "" {
		return ""
	}

	prefix := ""
	name := trimmed
	if idx := strings.Index(trimmed, "/"); idx > 0 && idx < len(trimmed)-1 {
		prefix = trimmed[:idx+1]
		name = trimmed[idx+1:]
	}

	matches := claudeStyleAliasPattern.FindStringSubmatch(name)
	if matches == nil {
		return ""
	}

	suffix := matches[5]
	if looksLikeClaudeDateSuffix(suffix) {
		return ""
	}

	alternate := fmt.Sprintf("%s-%s-%s%s", matches[1], matches[2], matches[4], suffix)
	return prefix + alternate
}

func requestedModelLookupCandidates(platform, requestedModel string) []string {
	seen := make(map[string]struct{}, 4)
	candidates := make([]string, 0, 4)
	add := func(model string) {
		model = strings.TrimSpace(model)
		if model == "" {
			return
		}
		if _, exists := seen[model]; exists {
			return
		}
		seen[model] = struct{}{}
		candidates = append(candidates, model)
	}

	add(requestedModel)
	normalized := normalizeRequestedModelForLookup(platform, requestedModel)
	add(normalized)

	for _, model := range []string{requestedModel, normalized} {
		canonical := CanonicalizePublicModel(model)
		add(canonical)
		add(publicModelStyleAlternate(canonical))
	}

	return candidates
}

func modelListContainsRequestedModel(modelIDs []string, requestedModel string) bool {
	if requestedModel == "" {
		return false
	}
	requestedCandidates := requestedModelLookupCandidates("", requestedModel)
	for _, model := range modelIDs {
		modelCandidates := requestedModelLookupCandidates("", model)
		for _, requested := range requestedCandidates {
			for _, candidate := range modelCandidates {
				if strings.EqualFold(candidate, requested) {
					return true
				}
			}
		}
	}
	return false
}
