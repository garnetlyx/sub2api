package service

import (
	"regexp"
	"strings"
)

var (
	// Matches public model IDs with an ambiguous numeric version separator,
	// e.g. gpt-5.5, gpt-5-5, minimax-m2.7, minimax-m2-7, claude-sonnet-4-6.
	numericVersionSepPattern = regexp.MustCompile(`^(.+-[A-Za-z]*\d+)([.-])(\d+)(-.+)?$`)
)

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

	canonical := canonicalizeNumericVersionSeparator(name)
	if canonical == "" {
		return trimmed
	}
	return prefix + canonical
}

func canonicalizeNumericVersionSeparator(model string) string {
	matches := numericVersionSepPattern.FindStringSubmatch(model)
	if matches == nil {
		return ""
	}

	suffix := matches[4]
	if looksLikeDateSuffix(suffix) {
		return ""
	}

	return matches[1] + "." + matches[3] + suffix
}

func looksLikeDateSuffix(suffix string) bool {
	if suffix == "" {
		return false
	}
	trimmed := strings.TrimPrefix(suffix, "-")
	for _, part := range strings.Split(trimmed, "-") {
		if len(part) < 8 {
			continue
		}
		datePart := part[:8]
		allDigits := true
		for _, ch := range datePart {
			if ch < '0' || ch > '9' {
				allDigits = false
				break
			}
		}
		if allDigits && (len(part) == 8 || part[8] == '-') {
			return true
		}
	}
	return false
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

	matches := numericVersionSepPattern.FindStringSubmatch(name)
	if matches == nil {
		return ""
	}

	suffix := matches[4]
	if looksLikeDateSuffix(suffix) {
		return ""
	}

	alternateSep := "-"
	if matches[2] == "-" {
		alternateSep = "."
	}
	alternate := matches[1] + alternateSep + matches[3] + suffix
	return prefix + alternate
}

func requestedModelLookupCandidates(platform, requestedModel string) []string {
	seen := make(map[string]struct{}, 8)
	candidates := make([]string, 0, 8)
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

	// Codex manifest endpoint (backend-api/codex/models) lists bare slugs
	// (gpt-5.6-luna) while clients request the -wm conversation variant
	// (gpt-5.6-luna-wm). normalizeCodexModel strips -wm at inference time;
	// mirror that here so live-model list matching stays consistent with
	// the inference path. -wm is an OpenAI-only slug suffix.
	for _, model := range []string{requestedModel, normalized} {
		if strings.HasSuffix(model, "-wm") && len(model) > len("-wm") {
			add(model[:len(model)-len("-wm")])
		}
	}

	return candidates
}

// dotHyphenAlternate flips the dot/hyphen between major and minor version
// numbers: "gpt-5.5" -> "gpt-5-5", "minimax-m2-7" -> "minimax-m2.7".
func dotHyphenAlternate(model string) string {
	return publicModelStyleAlternate(model)
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
