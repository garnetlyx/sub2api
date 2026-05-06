package service

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
)

const KnownIssueModelExclusionsSettingKey = "known_issue_model_exclusions"

type KnownIssueModelExclusion struct {
	Model        string `json:"model"`
	Platform     string `json:"platform,omitempty"`
	AccountName  string `json:"account_name,omitempty"`
	AccountID    int64  `json:"account_id,omitempty"`
	Endpoint     string `json:"endpoint,omitempty"`
	Capability   string `json:"capability,omitempty"`
	Evidence     string `json:"evidence"`
	LastVerified string `json:"last_verified"`
	RemoveWhen   string `json:"remove_when"`
}

type KnownIssueModelExclusionPolicy struct {
	entries []KnownIssueModelExclusion
}

type KnownIssueModelExclusionMatch struct {
	Entry KnownIssueModelExclusion
}

func LoadKnownIssueModelExclusionPolicy(ctx context.Context, settingRepo SettingRepository) KnownIssueModelExclusionPolicy {
	if settingRepo == nil {
		return KnownIssueModelExclusionPolicy{}
	}
	raw, err := settingRepo.GetValue(ctx, KnownIssueModelExclusionsSettingKey)
	if err != nil {
		if !errors.Is(err, ErrSettingNotFound) {
			slog.Warn("known_issue_model_exclusions_load_failed", "error", err)
		}
		return KnownIssueModelExclusionPolicy{}
	}
	entries, err := parseKnownIssueModelExclusions(raw)
	if err != nil {
		slog.Warn("known_issue_model_exclusions_parse_failed", "error", err)
		return KnownIssueModelExclusionPolicy{}
	}
	return KnownIssueModelExclusionPolicy{entries: entries}
}

func parseKnownIssueModelExclusions(raw string) ([]KnownIssueModelExclusion, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var entries []KnownIssueModelExclusion
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return nil, err
	}
	out := make([]KnownIssueModelExclusion, 0, len(entries))
	for _, entry := range entries {
		entry.Model = CanonicalizePublicModel(entry.Model)
		entry.Platform = strings.TrimSpace(entry.Platform)
		entry.AccountName = strings.TrimSpace(entry.AccountName)
		entry.Endpoint = strings.TrimSpace(entry.Endpoint)
		entry.Capability = strings.TrimSpace(entry.Capability)
		entry.Evidence = strings.TrimSpace(entry.Evidence)
		entry.LastVerified = strings.TrimSpace(entry.LastVerified)
		entry.RemoveWhen = strings.TrimSpace(entry.RemoveWhen)
		if entry.Model == "" || entry.Evidence == "" || entry.LastVerified == "" || entry.RemoveWhen == "" {
			continue
		}
		out = append(out, entry)
	}
	return out, nil
}

func (p KnownIssueModelExclusionPolicy) IsZero() bool {
	return len(p.entries) == 0
}

func (p KnownIssueModelExclusionPolicy) Excludes(model string, account *Account, endpoint string, capability string) (KnownIssueModelExclusionMatch, bool) {
	model = CanonicalizePublicModel(model)
	endpoint = strings.TrimSpace(endpoint)
	capability = strings.TrimSpace(capability)
	if model == "" || len(p.entries) == 0 {
		return KnownIssueModelExclusionMatch{}, false
	}
	for _, entry := range p.entries {
		if !modelListContainsRequestedModel([]string{entry.Model}, model) {
			continue
		}
		if entry.Platform != "" && (account == nil || !strings.EqualFold(entry.Platform, account.Platform)) {
			continue
		}
		if entry.AccountID > 0 && (account == nil || entry.AccountID != account.ID) {
			continue
		}
		if entry.AccountName != "" && (account == nil || !strings.EqualFold(entry.AccountName, account.Name)) {
			continue
		}
		if entry.Endpoint != "" && !strings.EqualFold(entry.Endpoint, endpoint) {
			continue
		}
		if entry.Capability != "" && !strings.EqualFold(entry.Capability, capability) {
			continue
		}
		return KnownIssueModelExclusionMatch{Entry: entry}, true
	}
	return KnownIssueModelExclusionMatch{}, false
}

func (p KnownIssueModelExclusionPolicy) FilterModels(models []string, account *Account, endpoint string, capability string) []string {
	if len(models) == 0 {
		return nil
	}
	out := make([]string, 0, len(models))
	for _, model := range models {
		if _, excluded := p.Excludes(model, account, endpoint, capability); excluded {
			continue
		}
		out = append(out, model)
	}
	return out
}
