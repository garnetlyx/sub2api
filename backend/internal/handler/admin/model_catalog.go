package admin

import (
	"sort"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/Wei-Shaw/sub2api/internal/pkg/geminicli"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

func canonicalizeModelIDs(modelIDs []string) []string {
	if len(modelIDs) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(modelIDs))
	out := make([]string, 0, len(modelIDs))
	for _, modelID := range modelIDs {
		canonical := service.CanonicalizePublicModel(modelID)
		if canonical == "" {
			continue
		}
		if _, exists := seen[canonical]; exists {
			continue
		}
		seen[canonical] = struct{}{}
		out = append(out, canonical)
	}
	sort.Strings(out)
	return out
}

func canonicalizeOpenAIModels(models []openai.Model) []openai.Model {
	if len(models) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(models))
	out := make([]openai.Model, 0, len(models))
	for _, model := range models {
		originalID := model.ID
		canonical := service.CanonicalizePublicModel(model.ID)
		if canonical == "" {
			continue
		}
		if _, exists := seen[canonical]; exists {
			continue
		}
		seen[canonical] = struct{}{}
		model.ID = canonical
		if model.DisplayName == "" || model.DisplayName == originalID {
			model.DisplayName = canonical
		}
		out = append(out, model)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func canonicalizeGeminiModels(models []geminicli.Model) []geminicli.Model {
	if len(models) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(models))
	out := make([]geminicli.Model, 0, len(models))
	for _, model := range models {
		originalID := model.ID
		canonical := service.CanonicalizePublicModel(model.ID)
		if canonical == "" {
			continue
		}
		if _, exists := seen[canonical]; exists {
			continue
		}
		seen[canonical] = struct{}{}
		model.ID = canonical
		if model.DisplayName == "" || model.DisplayName == originalID {
			model.DisplayName = canonical
		}
		out = append(out, model)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func canonicalizeClaudeModels(models []claude.Model) []claude.Model {
	if len(models) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(models))
	out := make([]claude.Model, 0, len(models))
	for _, model := range models {
		originalID := model.ID
		canonical := service.CanonicalizePublicModel(model.ID)
		if canonical == "" {
			continue
		}
		if _, exists := seen[canonical]; exists {
			continue
		}
		seen[canonical] = struct{}{}
		model.ID = canonical
		if model.DisplayName == "" || model.DisplayName == originalID {
			model.DisplayName = canonical
		}
		out = append(out, model)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func canonicalizeAntigravityModels(models []antigravity.ClaudeModel) []antigravity.ClaudeModel {
	if len(models) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(models))
	out := make([]antigravity.ClaudeModel, 0, len(models))
	for _, model := range models {
		originalID := model.ID
		canonical := service.CanonicalizePublicModel(model.ID)
		if canonical == "" {
			continue
		}
		if _, exists := seen[canonical]; exists {
			continue
		}
		seen[canonical] = struct{}{}
		model.ID = canonical
		if model.DisplayName == "" || model.DisplayName == originalID {
			model.DisplayName = canonical
		}
		out = append(out, model)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
