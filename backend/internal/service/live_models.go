package service

import (
	"context"
	"crypto/sha256"
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
	"github.com/imroc/req/v3"
)

type LiveModelSource struct {
	Account    *Account
	Endpoint   string
	Capability string
	Models     []string
}

type liveModelSourceLoader func(context.Context, Account) (LiveModelSource, error)

const liveModelSourceCacheKeyPrefix = "live-model-source|"

var chatGPTLiveModelEndpoints = []string{
	"https://chatgpt.com/backend-api/models?history_and_training_disabled=false",
	"https://chatgpt.com/backend-api/models",
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

func publicizeLiveModelList(account *Account, models []string) []string {
	if account == nil || len(models) == 0 {
		return canonicalLiveModelList(models)
	}
	upstreamModels := account.GetUpstreamModels()
	if len(upstreamModels) == 0 {
		return canonicalLiveModelList(models)
	}

	reverse := make(map[string]string, len(upstreamModels))
	for publicModel, upstreamModel := range upstreamModels {
		publicModel = strings.TrimSpace(publicModel)
		upstreamModel = strings.TrimSpace(upstreamModel)
		if publicModel == "" || upstreamModel == "" {
			continue
		}
		key := strings.ToLower(upstreamModel)
		if _, exists := reverse[key]; !exists {
			reverse[key] = publicModel
		}
	}

	out := make([]string, 0, len(models))
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		if publicModel, ok := reverse[strings.ToLower(model)]; ok {
			out = append(out, publicModel)
			continue
		}
		out = append(out, model)
	}
	return canonicalLiveModelList(out)
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

func decodeChatGPTModelIDs(body []byte) []string {
	var payload any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil
	}

	models := make(map[string]struct{})
	var walk func(any)
	walk = func(value any) {
		switch node := value.(type) {
		case map[string]any:
			if slug, ok := node["slug"].(string); ok {
				slug = strings.TrimSpace(slug)
				if slug != "" {
					models[slug] = struct{}{}
				}
			}
			for _, child := range node {
				walk(child)
			}
		case []any:
			for _, child := range node {
				walk(child)
			}
		}
	}
	walk(payload)

	out := make([]string, 0, len(models))
	for model := range models {
		out = append(out, model)
	}
	return out
}

func cloneAccountForLiveSource(account Account) *Account {
	cloned := account
	return &cloned
}

func cloneLiveModelSource(source LiveModelSource) LiveModelSource {
	cloned := source
	if source.Account != nil {
		account := *source.Account
		cloned.Account = &account
	}
	if source.Models != nil {
		cloned.Models = append([]string(nil), source.Models...)
	}
	return cloned
}

func liveModelSourceCacheKey(account Account) string {
	signature := map[string]any{
		"id":          account.ID,
		"name":        account.Name,
		"platform":    account.Platform,
		"type":        account.Type,
		"credentials": account.Credentials,
		"extra":       account.Extra,
		"proxy_id":    account.ProxyID,
		"proxy_url":   proxyURLForAccount(&account),
		"updated_at":  account.UpdatedAt.UnixNano(),
	}
	data, err := json.Marshal(signature)
	if err != nil {
		data = []byte(fmt.Sprintf("%d|%s|%s|%s|%d", account.ID, account.Name, account.Platform, account.Type, account.UpdatedAt.UnixNano()))
	}
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%s%d|%s|%x", liveModelSourceCacheKeyPrefix, account.ID, strings.TrimSpace(account.Platform), sum[:8])
}

func (s *GatewayService) liveModelSourceCacheTTL() time.Duration {
	if s == nil {
		return 0
	}
	if s.modelsListCacheTTL > 0 {
		return s.modelsListCacheTTL
	}
	return resolveModelsListCacheTTL(s.cfg)
}

func (s *GatewayService) cachedLiveModelSourceForAccountWithLoader(ctx context.Context, account Account, loader liveModelSourceLoader) (LiveModelSource, error) {
	if s == nil {
		return LiveModelSource{}, errors.New("gateway service is nil")
	}
	if loader == nil {
		return LiveModelSource{}, errors.New("live model source loader is nil")
	}

	cache := s.modelsListCache
	ttl := s.liveModelSourceCacheTTL()
	if cache == nil || ttl <= 0 {
		return loader(ctx, account)
	}

	key := liveModelSourceCacheKey(account)
	if cached, ok := cache.Get(key); ok {
		if source, ok := cached.(LiveModelSource); ok {
			modelsListCacheHitTotal.Add(1)
			return cloneLiveModelSource(source), nil
		}
		cache.Delete(key)
	}
	modelsListCacheMissTotal.Add(1)

	value, err, _ := s.liveModelSourceSF.Do(key, func() (any, error) {
		if cached, ok := cache.Get(key); ok {
			if source, ok := cached.(LiveModelSource); ok {
				return cloneLiveModelSource(source), nil
			}
			cache.Delete(key)
		}

		source, err := loader(ctx, account)
		if err != nil {
			return LiveModelSource{}, err
		}
		source = cloneLiveModelSource(source)
		if strings.TrimSpace(source.Endpoint) != "" {
			cache.Set(key, source, ttl)
			modelsListCacheStoreTotal.Add(1)
		}
		return source, nil
	})
	if err != nil {
		return LiveModelSource{}, err
	}
	source, ok := value.(LiveModelSource)
	if !ok {
		return LiveModelSource{}, errors.New("cached live model source has invalid type")
	}
	return cloneLiveModelSource(source), nil
}

func (s *GatewayService) InvalidateLiveModelSourceCache(accountIDs ...int64) {
	if s == nil || s.modelsListCache == nil {
		return
	}
	if len(accountIDs) == 0 {
		for key := range s.modelsListCache.Items() {
			if strings.HasPrefix(key, liveModelSourceCacheKeyPrefix) {
				s.modelsListCache.Delete(key)
			}
		}
		return
	}

	prefixes := make([]string, 0, len(accountIDs))
	for _, accountID := range accountIDs {
		if accountID <= 0 {
			continue
		}
		prefixes = append(prefixes, fmt.Sprintf("%s%d|", liveModelSourceCacheKeyPrefix, accountID))
	}
	if len(prefixes) == 0 {
		return
	}
	for key := range s.modelsListCache.Items() {
		for _, prefix := range prefixes {
			if strings.HasPrefix(key, prefix) {
				s.modelsListCache.Delete(key)
				break
			}
		}
	}
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
	if isInternalLiteLLMBridgeOnlyAccount(acc) {
		return nil, "", "", nil
	}
	var (
		models     []string
		endpoint   string
		capability string
		err        error
	)
	switch {
	case acc.IsOpenAIApiKey() && !isInternalLiteLLMBridgeOnlyAccount(acc):
		models, err = s.liveOpenAICompatibleModels(ctx, acc)
		endpoint = "openai-compatible"
		capability = "chat"
	case acc.IsAnthropic() && acc.Type == AccountTypeAPIKey:
		models, err = s.liveAnthropicModels(ctx, acc)
		endpoint = "anthropic"
		capability = "messages"
	case acc.IsGemini() && acc.Type == AccountTypeAPIKey:
		models, err = s.liveGeminiModels(ctx, acc)
		endpoint = "gemini"
		capability = "generateContent"
	case acc.Platform == PlatformAntigravity:
		return nil, "antigravity", "messages", nil
	default:
		return nil, "", "", nil
	}
	if err != nil {
		return nil, endpoint, capability, err
	}
	return publicizeLiveModelList(acc, models), endpoint, capability, nil
}

func (s *GatewayService) liveModelSourceForAccount(ctx context.Context, account Account) (LiveModelSource, error) {
	models, endpoint, capability, err := s.liveAccountModels(ctx, account)
	if err != nil {
		return LiveModelSource{}, err
	}
	return LiveModelSource{
		Account:    cloneAccountForLiveSource(account),
		Endpoint:   endpoint,
		Capability: capability,
		Models:     models,
	}, nil
}

func (s *GatewayService) LiveModelSourceForAccount(ctx context.Context, account Account) (LiveModelSource, error) {
	return s.cachedLiveModelSourceForAccountWithLoader(ctx, account, s.liveModelSourceForAccount)
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
			source, err := s.cachedLiveModelSourceForAccountWithLoader(ctx, account, s.liveModelSourceForAccount)
			if err != nil {
				slog.Warn("live_models_lookup_failed",
					"account_id", account.ID,
					"account_name", account.Name,
					"platform", account.Platform,
					"error", err,
				)
				return
			}
			if len(source.Models) == 0 {
				return
			}
			sourceCh <- source
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
	items, err := kiro.ListModels(ctx, httpClient, region, accessToken, account.GetExtraString("profile_arn"))
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

func (s *OpenAIGatewayService) liveOpenAIOAuthModels(ctx context.Context, account *Account) ([]string, error) {
	if s == nil {
		return nil, errors.New("openai gateway service is nil")
	}
	token, _, err := s.GetAccessToken(ctx, account)
	if err != nil {
		return nil, err
	}

	client := req.C().SetTimeout(30 * time.Second).ImpersonateChrome()
	if proxyURL := proxyURLForAccount(account); proxyURL != "" {
		client.SetProxyURL(proxyURL)
	}

	var lastErr error
	for _, endpoint := range chatGPTLiveModelEndpoints {
		req := client.R().
			SetContext(ctx).
			SetHeader("Authorization", "Bearer "+token).
			SetHeader("Origin", "https://chatgpt.com").
			SetHeader("Referer", "https://chatgpt.com/").
			SetHeader("Accept", "application/json").
			SetHeader("sec-fetch-mode", "cors").
			SetHeader("sec-fetch-site", "same-origin").
			SetHeader("sec-fetch-dest", "empty")
		if chatGPTAccountID := strings.TrimSpace(account.GetChatGPTAccountID()); chatGPTAccountID != "" {
			req.SetHeader("chatgpt-account-id", chatGPTAccountID)
		}
		resp, err := req.Get(endpoint)
		if err != nil {
			lastErr = err
			continue
		}
		if !resp.IsSuccessState() {
			lastErr = fmt.Errorf("models lookup failed: status %d", resp.StatusCode)
			continue
		}
		models := canonicalLiveModelList(decodeChatGPTModelIDs([]byte(resp.String())))
		if len(models) == 0 {
			lastErr = errors.New("models lookup returned no models")
			continue
		}
		return models, nil
	}

	if lastErr == nil {
		lastErr = errors.New("models lookup returned no models")
	}
	return nil, lastErr
}

func (s *OpenAIGatewayService) liveModelSourceForAccount(ctx context.Context, account Account) (LiveModelSource, error) {
	acc := cloneAccountForLiveSource(account)
	var (
		models     []string
		endpoint   string
		capability = "chat"
		err        error
	)
	switch {
	case acc.IsCopilot():
		models, err = s.liveCopilotModels(ctx, acc)
		endpoint = "copilot"
	case acc.IsKiro():
		models, err = s.liveKiroModels(ctx, acc)
		endpoint = "kiro"
	case acc.IsOpenAIOAuth():
		models, err = s.liveOpenAIOAuthModels(ctx, acc)
		endpoint = "chatgpt"
	case acc.IsOpenAIApiKey() && !isInternalLiteLLMBridgeOnlyAccount(acc):
		models, err = s.liveOpenAICompatibleModels(ctx, acc)
		endpoint = "openai-compatible"
	default:
		return LiveModelSource{Account: acc}, nil
	}
	if err != nil {
		return LiveModelSource{}, err
	}
	models = publicizeLiveModelList(acc, models)
	return LiveModelSource{
		Account:    acc,
		Endpoint:   endpoint,
		Capability: capability,
		Models:     models,
	}, nil
}

func (s *OpenAIGatewayService) LiveModelSourceForAccount(ctx context.Context, account Account) (LiveModelSource, error) {
	return s.cachedLiveModelSourceForAccount(ctx, account)
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
			source, err := s.cachedLiveModelSourceForAccount(ctx, account)
			if err != nil {
				slog.Warn("openai_live_models_lookup_failed",
					"account_id", account.ID,
					"account_name", account.Name,
					"platform", account.Platform,
					"error", err,
				)
				return
			}
			if len(source.Models) == 0 {
				return
			}
			sourceCh <- source
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

func (s *OpenAIGatewayService) cachedLiveModelSourceForAccount(ctx context.Context, account Account) (LiveModelSource, error) {
	if s == nil {
		return LiveModelSource{}, errors.New("openai gateway service is nil")
	}
	if s.gatewayService == nil {
		return s.liveModelSourceForAccount(ctx, account)
	}
	return s.gatewayService.cachedLiveModelSourceForAccountWithLoader(ctx, account, s.liveModelSourceForAccount)
}
