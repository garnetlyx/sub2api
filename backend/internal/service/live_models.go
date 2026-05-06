package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/copilot"
	"github.com/Wei-Shaw/sub2api/internal/pkg/geminicli"
	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
)

type LiveModelSource struct {
	Account    *Account
	Endpoint   string
	Capability string
	Models     []string
}

func canonicalLiveModelList(models []string) []string {
	seen := make(map[string]struct{}, len(models))
	out := make([]string, 0, len(models))
	for _, model := range models {
		canonical := CanonicalizePublicModel(model)
		if canonical == "" {
			continue
		}
		key := strings.ToLower(canonical)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, canonical)
	}
	sort.Strings(out)
	return out
}

func MergeLiveModelSources(sources []LiveModelSource, policy KnownIssueModelExclusionPolicy) []string {
	modelSet := make(map[string]struct{})
	for _, source := range sources {
		for _, model := range canonicalLiveModelList(source.Models) {
			if _, excluded := policy.Excludes(model, source.Account, source.Endpoint, source.Capability); excluded {
				continue
			}
			modelSet[model] = struct{}{}
		}
	}
	out := make([]string, 0, len(modelSet))
	for model := range modelSet {
		out = append(out, model)
	}
	sort.Strings(out)
	return out
}

func liveOpenAIModelEndpoint(baseURL string) string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		return ""
	}
	if strings.HasSuffix(base, "/models") {
		return base
	}
	if strings.HasSuffix(base, "/v1") {
		return base + "/models"
	}
	return base + "/v1/models"
}

func liveAnthropicModelEndpoint(baseURL string) string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		return ""
	}
	if strings.HasSuffix(base, "/models") {
		return base
	}
	if strings.HasSuffix(base, "/v1") {
		return base + "/models"
	}
	return base + "/v1/models"
}

func liveGeminiModelEndpoint(baseURL string) string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		return ""
	}
	if strings.HasSuffix(base, "/models") || strings.HasSuffix(base, "/v1beta/models") {
		return base
	}
	if strings.HasSuffix(base, "/v1beta") {
		return base + "/models"
	}
	return base + "/v1beta/models"
}

func decodeOpenAIModelIDs(body []byte) []string {
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil
	}
	models := make([]string, 0, len(payload.Data))
	for _, item := range payload.Data {
		if id := strings.TrimSpace(item.ID); id != "" {
			models = append(models, id)
		}
	}
	return models
}

func decodeAnthropicModelIDs(body []byte) []string {
	return decodeOpenAIModelIDs(body)
}

func decodeGeminiModelIDs(body []byte) []string {
	var payload struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil
	}
	models := make([]string, 0, len(payload.Models))
	for _, item := range payload.Models {
		name := strings.TrimSpace(item.Name)
		name = strings.TrimPrefix(name, "models/")
		if name != "" {
			models = append(models, name)
		}
	}
	return models
}

func cloneAccountForLiveSource(account Account) *Account {
	cloned := account
	return &cloned
}

func proxyURLForAccount(account *Account) string {
	if account != nil && account.ProxyID != nil && account.Proxy != nil {
		return account.Proxy.URL()
	}
	return ""
}

func (s *GatewayService) doLiveModelsRequest(ctx context.Context, account *Account, method string, endpoint string, configure func(*http.Request)) ([]byte, int, error) {
	if s == nil || s.httpUpstream == nil {
		return nil, 0, errors.New("http upstream is not configured")
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	if configure != nil {
		configure(req)
	}
	accountID := int64(0)
	concurrency := 1
	if account != nil {
		accountID = account.ID
		concurrency = account.Concurrency
	}
	resp, err := s.httpUpstream.Do(req, proxyURLForAccount(account), accountID, concurrency)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return body, resp.StatusCode, fmt.Errorf("models lookup failed: status %d", resp.StatusCode)
	}
	return body, resp.StatusCode, nil
}

func (s *GatewayService) liveOpenAICompatibleModels(ctx context.Context, account *Account) ([]string, error) {
	endpoint := liveOpenAIModelEndpoint(account.GetOpenAIBaseURL())
	validated, err := s.validateUpstreamBaseURL(endpoint)
	if err != nil {
		return nil, err
	}
	apiKey := strings.TrimSpace(account.GetOpenAIApiKey())
	if apiKey == "" {
		return nil, errors.New("openai api_key is not configured")
	}
	body, _, err := s.doLiveModelsRequest(ctx, account, http.MethodGet, validated, func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	})
	if err != nil {
		return nil, err
	}
	return canonicalLiveModelList(decodeOpenAIModelIDs(body)), nil
}

func (s *GatewayService) liveAnthropicModels(ctx context.Context, account *Account) ([]string, error) {
	endpoint := liveAnthropicModelEndpoint(account.GetBaseURL())
	validated, err := s.validateUpstreamBaseURL(endpoint)
	if err != nil {
		return nil, err
	}
	token, tokenType, err := s.GetAccessToken(ctx, account)
	if err != nil {
		return nil, err
	}
	body, _, err := s.doLiveModelsRequest(ctx, account, http.MethodGet, validated, func(req *http.Request) {
		if tokenType == "oauth" {
			setHeaderRaw(req.Header, "authorization", "Bearer "+token)
		} else {
			setHeaderRaw(req.Header, "x-api-key", token)
		}
		setHeaderRaw(req.Header, "anthropic-version", "2023-06-01")
	})
	if err != nil {
		return nil, err
	}
	return canonicalLiveModelList(decodeAnthropicModelIDs(body)), nil
}

func (s *GatewayService) liveGeminiModels(ctx context.Context, account *Account) ([]string, error) {
	endpoint := liveGeminiModelEndpoint(account.GetGeminiBaseURL(geminicli.AIStudioBaseURL))
	validated, err := s.validateUpstreamBaseURL(endpoint)
	if err != nil {
		return nil, err
	}
	switch account.Type {
	case AccountTypeAPIKey:
		apiKey := strings.TrimSpace(account.GetCredential("api_key"))
		if apiKey == "" {
			return nil, errors.New("gemini api_key is not configured")
		}
		body, _, err := s.doLiveModelsRequest(ctx, account, http.MethodGet, validated, func(req *http.Request) {
			req.Header.Set("x-goog-api-key", apiKey)
		})
		if err != nil {
			return nil, err
		}
		return canonicalLiveModelList(decodeGeminiModelIDs(body)), nil
	default:
		return nil, fmt.Errorf("live gemini models unsupported for account type %s", account.Type)
	}
}

func (s *GatewayService) liveAccountModels(ctx context.Context, account Account) ([]string, string, string, error) {
	acc := cloneAccountForLiveSource(account)
	switch {
	case acc.IsOpenAIApiKey() && !isInternalLiteLLMBridgeOnlyAccount(acc):
		models, err := s.liveOpenAICompatibleModels(ctx, acc)
		return models, "openai-compatible", "chat", err
	case acc.IsAnthropic() && acc.Type == AccountTypeAPIKey:
		models, err := s.liveAnthropicModels(ctx, acc)
		return models, "anthropic", "messages", err
	case acc.IsGemini() && acc.Type == AccountTypeAPIKey:
		models, err := s.liveGeminiModels(ctx, acc)
		return models, "gemini", "generateContent", err
	case acc.Platform == PlatformAntigravity:
		return nil, "antigravity", "messages", nil
	default:
		return nil, "", "", nil
	}
}

func (s *GatewayService) GetLiveModelSources(ctx context.Context, groupID *int64, platform string) []LiveModelSource {
	accounts, err := s.listSchedulableAccountsForModels(ctx, groupID, platform)
	if err != nil || len(accounts) == 0 {
		if err != nil {
			slog.Warn("live_models_account_list_failed", "platform", platform, "error", err)
		}
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	sourceCh := make(chan LiveModelSource, len(accounts))
	for _, account := range accounts {
		account := account
		wg.Add(1)
		go func() {
			defer wg.Done()
			models, endpoint, capability, err := s.liveAccountModels(ctx, account)
			if err != nil {
				slog.Warn("live_models_lookup_failed",
					"account_id", account.ID,
					"account_name", account.Name,
					"platform", account.Platform,
					"error", err,
				)
				return
			}
			if len(models) == 0 {
				return
			}
			sourceCh <- LiveModelSource{
				Account:    cloneAccountForLiveSource(account),
				Endpoint:   endpoint,
				Capability: capability,
				Models:     models,
			}
		}()
	}
	wg.Wait()
	close(sourceCh)

	sources := make([]LiveModelSource, 0, len(accounts))
	for source := range sourceCh {
		sources = append(sources, source)
	}
	return sources
}

func (s *GatewayService) GetLiveAvailableModels(ctx context.Context, groupID *int64, platform string) []string {
	return MergeLiveModelSources(s.GetLiveModelSources(ctx, groupID, platform), LoadKnownIssueModelExclusionPolicy(ctx, s.settingService.SettingRepoOrNil()))
}

func (s *OpenAIGatewayService) liveCopilotModels(ctx context.Context, account *Account) ([]string, error) {
	if s == nil || s.copilotTokenProvider == nil {
		return nil, errors.New("copilot token provider is not configured")
	}
	accessToken, err := s.copilotTokenProvider.GetAccessToken(ctx, account)
	if err != nil {
		return nil, err
	}
	httpClient, err := copilot.NewHTTPClient(s.copilotTokenProvider.proxyURLForAccount(ctx, account))
	if err != nil {
		return nil, err
	}
	models, err := copilot.ListModels(ctx, httpClient, accessToken)
	if err != nil {
		return nil, err
	}
	return canonicalLiveModelList(models), nil
}

func (s *OpenAIGatewayService) liveKiroModels(ctx context.Context, account *Account) ([]string, error) {
	if s == nil || s.kiroTokenProvider == nil {
		return nil, errors.New("kiro token provider is not configured")
	}
	accessToken, err := s.kiroTokenProvider.GetAccessToken(ctx, account)
	if err != nil {
		return nil, err
	}
	region := strings.TrimSpace(account.GetExtraString("region"))
	if region == "" {
		region = strings.TrimSpace(account.GetCredential("region"))
	}
	if region == "" {
		region = "us-east-1"
	}
	httpClient, err := kiro.NewHTTPClient(s.kiroTokenProvider.proxyURLForAccount(ctx, account))
	if err != nil {
		return nil, err
	}
	items, err := kiro.ListModels(ctx, httpClient, region, accessToken)
	if err != nil {
		return nil, err
	}
	models := make([]string, 0, len(items))
	for _, item := range items {
		if modelID := strings.TrimSpace(item.ModelID); modelID != "" {
			models = append(models, modelID)
		}
	}
	return canonicalLiveModelList(models), nil
}

func (s *OpenAIGatewayService) liveOpenAICompatibleModels(ctx context.Context, account *Account) ([]string, error) {
	gateway := &GatewayService{httpUpstream: s.httpUpstream, cfg: s.cfg}
	return gateway.liveOpenAICompatibleModels(ctx, account)
}

func (s *OpenAIGatewayService) GetLiveModelSources(ctx context.Context, groupID *int64) []LiveModelSource {
	accounts, err := s.listSchedulableAccounts(ctx, groupID)
	if err != nil || len(accounts) == 0 {
		if err != nil {
			slog.Warn("openai_live_models_account_list_failed", "error", err)
		}
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	sourceCh := make(chan LiveModelSource, len(accounts))
	for _, account := range accounts {
		account := account
		wg.Add(1)
		go func() {
			defer wg.Done()
			acc := cloneAccountForLiveSource(account)
			var models []string
			var endpoint string
			var err error
			switch {
			case acc.IsCopilot():
				models, err = s.liveCopilotModels(ctx, acc)
				endpoint = "copilot"
			case acc.IsKiro():
				models, err = s.liveKiroModels(ctx, acc)
				endpoint = "kiro"
			case acc.IsOpenAIApiKey() && !isInternalLiteLLMBridgeOnlyAccount(acc):
				models, err = s.liveOpenAICompatibleModels(ctx, acc)
				endpoint = "openai-compatible"
			default:
				return
			}
			if err != nil {
				slog.Warn("openai_live_models_lookup_failed",
					"account_id", acc.ID,
					"account_name", acc.Name,
					"platform", acc.Platform,
					"error", err,
				)
				return
			}
			if len(models) == 0 {
				return
			}
			sourceCh <- LiveModelSource{
				Account:    acc,
				Endpoint:   endpoint,
				Capability: "chat",
				Models:     models,
			}
		}()
	}
	wg.Wait()
	close(sourceCh)

	sources := make([]LiveModelSource, 0, len(accounts))
	for source := range sourceCh {
		sources = append(sources, source)
	}
	return sources
}

func AppendLiveModels(dst []LiveModelSource, src []LiveModelSource) []LiveModelSource {
	return append(dst, src...)
}
