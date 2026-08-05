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
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/Wei-Shaw/sub2api/internal/pkg/copilot"
	"github.com/Wei-Shaw/sub2api/internal/pkg/geminicli"
	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"github.com/imroc/req/v3"
	gocache "github.com/patrickmn/go-cache"
)

type LiveModelSource struct {
	Account        *Account
	Endpoint       string
	Capability     string
	Models         []string
	UpstreamModels map[string]string
}

type liveModelSourceLoader func(context.Context, Account) (LiveModelSource, error)

const liveModelSourceCacheKeyPrefix = "live-model-source|"

type liveModelCatalog struct {
	Models         []string
	UpstreamModels map[string]string
}

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

func cleanLiveModelList(models []string) []string {
	seen := make(map[string]struct{}, len(models))
	out := make([]string, 0, len(models))
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		key := strings.ToLower(model)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, model)
	}
	sort.Strings(out)
	return out
}

type liveModelBinding struct {
	Public   string
	Upstream string
	Explicit bool
}

func buildExplicitUpstreamMappings(account *Account) map[string]string {
	if account == nil {
		return nil
	}
	out := make(map[string]string)
	for publicModel, upstreamModel := range account.GetUpstreamModels() {
		publicModel = strings.TrimSpace(publicModel)
		upstreamModel = strings.TrimSpace(upstreamModel)
		if publicModel == "" || upstreamModel == "" {
			continue
		}
		out[publicModel] = upstreamModel
	}
	for publicModel, upstreamModel := range account.GetModelMapping() {
		publicModel = strings.TrimSpace(publicModel)
		upstreamModel = strings.TrimSpace(upstreamModel)
		if publicModel == "" || upstreamModel == "" || strings.Contains(publicModel, "*") {
			continue
		}
		if _, exists := out[publicModel]; !exists {
			out[publicModel] = upstreamModel
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func upstreamModelsEquivalent(a, b string) bool {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	if a == "" || b == "" {
		return false
	}
	if strings.EqualFold(a, b) {
		return true
	}
	return modelListContainsRequestedModel([]string{a}, b)
}

func publicizeLiveModelCatalog(account *Account, rawModels []string) liveModelCatalog {
	rawModels = cleanLiveModelList(rawModels)
	bindings := make(map[string]liveModelBinding, len(rawModels))
	add := func(publicModel, upstreamModel string, explicit bool) {
		publicModel = CanonicalizePublicModel(publicModel)
		upstreamModel = strings.TrimSpace(upstreamModel)
		if publicModel == "" || upstreamModel == "" {
			return
		}
		key := strings.ToLower(publicModel)
		if existing, exists := bindings[key]; !exists || (explicit && !existing.Explicit) {
			bindings[key] = liveModelBinding{Public: publicModel, Upstream: upstreamModel, Explicit: explicit}
		}
	}

	explicitOnly := account != nil && account.PublicModelsExplicitOnly()
	if !explicitOnly {
		for _, rawModel := range rawModels {
			add(rawModel, rawModel, false)
		}
	}

	for publicModel, upstreamModel := range buildExplicitUpstreamMappings(account) {
		matched := false
		for _, rawModel := range rawModels {
			if upstreamModelsEquivalent(upstreamModel, rawModel) {
				add(publicModel, rawModel, true)
				matched = true
				break
			}
		}
		if !matched {
			if account != nil {
				slog.Debug("gateway.model_from_config_fallback",
					"account_id", account.ID,
					"public_model", publicModel,
					"upstream_model", upstreamModel,
				)
			}
			add(publicModel, upstreamModel, true)
		}
	}

	models := make([]string, 0, len(bindings))
	upstreamModels := make(map[string]string, len(bindings))
	for _, binding := range bindings {
		models = append(models, binding.Public)
		upstreamModels[binding.Public] = binding.Upstream
	}
	sort.Strings(models)
	return liveModelCatalog{Models: models, UpstreamModels: upstreamModels}
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

// liveOpenAIModelFallbackEndpoint returns the /models path without /v1/ for
// providers whose base URL does not follow the standard OpenAI /v1 convention.
func liveOpenAIModelFallbackEndpoint(baseURL string) string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		return ""
	}
	return base + "/models"
}

// liveAnthropicCrossProtocolEndpoint strips the last path segment from an
// Anthropic base URL and returns an OpenAI-compatible /v1/models endpoint.
// Returns empty string if the base URL has no path to strip (e.g. a bare domain).
// Example: "https://api.deepseek.com/anthropic" → "https://api.deepseek.com/v1/models"
func liveAnthropicCrossProtocolEndpoint(baseURL string) string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		return ""
	}
	parsed, err := url.Parse(base)
	if err != nil {
		return ""
	}
	path := strings.TrimRight(parsed.Path, "/")
	if path == "" {
		return ""
	}
	lastSlash := strings.LastIndex(path, "/")
	if lastSlash > 0 {
		parsed.Path = path[:lastSlash]
	} else {
		parsed.Path = ""
	}
	return parsed.String() + "/v1/models"
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
	if source.UpstreamModels != nil {
		cloned.UpstreamModels = make(map[string]string, len(source.UpstreamModels))
		for publicModel, upstreamModel := range source.UpstreamModels {
			cloned.UpstreamModels[publicModel] = upstreamModel
		}
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

func (s *GatewayService) liveModelSourceLookupTimeout() time.Duration {
	if s == nil {
		return defaultModelsListLookupTimeout
	}
	return resolveModelsListLookupTimeout(s.cfg)
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

		lookupCtx := ctx
		cancel := func() {}
		timeout := s.liveModelSourceLookupTimeout()
		if timeout > 0 {
			lookupCtx, cancel = context.WithTimeout(ctx, timeout)
		}
		defer cancel()

		startedAt := time.Now()
		source, err := loader(lookupCtx, account)
		if err != nil {
			duration := time.Since(startedAt).Truncate(time.Millisecond)
			return LiveModelSource{}, fmt.Errorf("live model source lookup failed after %s (timeout %s): %w", duration, timeout, err)
		}
		source = cloneLiveModelSource(source)
		if strings.TrimSpace(source.Endpoint) == "" {
			if source.Account != nil {
				slog.Warn("gateway.live_model_source_cache_skip_empty_endpoint",
					"account_id", source.Account.ID,
					"account_name", source.Account.Name,
					"platform", source.Account.Platform,
					"account_type", source.Account.Type,
					"models_count", len(source.Models),
				)
			}
			return source, nil
		}
		cache.Set(key, source, ttl)
		modelsListCacheStoreTotal.Add(1)
		if source.Account != nil {
			slog.Debug("gateway.live_model_source_cache_store",
				"account_id", source.Account.ID,
				"account_name", source.Account.Name,
				"platform", source.Account.Platform,
				"account_type", source.Account.Type,
				"endpoint", source.Endpoint,
				"models_count", len(source.Models),
			)
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
	if resp == nil {
		return nil, 0, errors.New("models lookup failed: empty response")
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
	authSetter := func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	body, statusCode, err := s.doLiveModelsRequest(ctx, account, http.MethodGet, validated, authSetter)
	if err != nil {
		if statusCode == 404 {
			fbEndpoint := liveOpenAIModelFallbackEndpoint(account.GetOpenAIBaseURL())
			if fbValidated, fbErr := s.validateUpstreamBaseURL(fbEndpoint); fbErr == nil && fbValidated != validated {
				if fbBody, _, fbErr := s.doLiveModelsRequest(ctx, account, http.MethodGet, fbValidated, authSetter); fbErr == nil {
					return cleanLiveModelList(decodeOpenAIModelIDs(fbBody)), nil
				}
			}
		}
		return nil, err
	}
	return cleanLiveModelList(decodeOpenAIModelIDs(body)), nil
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
	configureAPIKey := func(req *http.Request) {
		if tokenType == "oauth" {
			setHeaderRaw(req.Header, "authorization", "Bearer "+token)
		} else {
			setHeaderRaw(req.Header, "x-api-key", token)
		}
		setHeaderRaw(req.Header, "anthropic-version", "2023-06-01")
	}
	body, statusCode, err := s.doLiveModelsRequest(ctx, account, http.MethodGet, validated, configureAPIKey)
	if err != nil {
		if statusCode == 401 && tokenType != "oauth" {
			configureBearer := func(req *http.Request) {
				setHeaderRaw(req.Header, "authorization", "Bearer "+token)
				setHeaderRaw(req.Header, "anthropic-version", "2023-06-01")
			}
			if fbBody, _, fbErr := s.doLiveModelsRequest(ctx, account, http.MethodGet, validated, configureBearer); fbErr == nil {
				return cleanLiveModelList(decodeAnthropicModelIDs(fbBody)), nil
			}
		}
		apiKey := strings.TrimSpace(account.GetCredential("api_key"))
		if cpEndpoint := liveAnthropicCrossProtocolEndpoint(account.GetBaseURL()); cpEndpoint != "" && apiKey != "" {
			if cpValidated, cpErr := s.validateUpstreamBaseURL(cpEndpoint); cpErr == nil && cpValidated != validated {
				slog.Debug("gateway.anthropic_cross_protocol_fallback",
					"account_id", account.ID,
					"account_name", account.Name,
					"anthropic_endpoint", validated,
					"cross_protocol_endpoint", cpValidated,
				)
				cpAuth := func(req *http.Request) {
					req.Header.Set("Authorization", "Bearer "+apiKey)
				}
				if cpBody, _, cpErr := s.doLiveModelsRequest(ctx, account, http.MethodGet, cpValidated, cpAuth); cpErr == nil {
					return cleanLiveModelList(decodeOpenAIModelIDs(cpBody)), nil
				}
			}
		}
		return nil, err
	}
	return cleanLiveModelList(decodeAnthropicModelIDs(body)), nil
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
		return cleanLiveModelList(decodeGeminiModelIDs(body)), nil
	default:
		return nil, fmt.Errorf("live gemini models unsupported for account type %s", account.Type)
	}
}

func shouldTryNextAntigravityLiveModelsEndpoint(status int) bool {
	return status == 0 ||
		status == http.StatusTooManyRequests ||
		status == http.StatusRequestTimeout ||
		status == http.StatusNotFound ||
		status >= 500
}

func (s *GatewayService) liveAntigravityModels(ctx context.Context, account *Account) ([]string, error) {
	if account == nil {
		return nil, errors.New("account is nil")
	}
	if s == nil || s.antigravityTokenProvider == nil {
		return nil, errors.New("antigravity token provider is not configured")
	}
	accessToken, err := s.antigravityTokenProvider.GetAccessToken(ctx, account)
	if err != nil {
		return nil, err
	}
	projectID := strings.TrimSpace(account.GetCredential("project_id"))
	body, err := json.Marshal(antigravity.FetchAvailableModelsRequest{Project: projectID})
	if err != nil {
		return nil, err
	}

	var lastErr error
	for _, baseURL := range antigravity.BaseURLs {
		endpoint := strings.TrimRight(baseURL, "/") + "/v1internal:fetchAvailableModels"
		validated, validateErr := s.validateUpstreamBaseURL(endpoint)
		if validateErr != nil {
			lastErr = validateErr
			continue
		}
		respBody, status, reqErr := s.doLiveModelsRequest(ctx, account, http.MethodPost, validated, func(req *http.Request) {
			req.Header.Set("Authorization", "Bearer "+accessToken)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("User-Agent", antigravity.GetUserAgent())
			req.Body = io.NopCloser(strings.NewReader(string(body)))
			req.ContentLength = int64(len(body))
		})
		if reqErr != nil {
			if shouldTryNextAntigravityLiveModelsEndpoint(status) {
				lastErr = reqErr
				continue
			}
			return nil, reqErr
		}
		var payload antigravity.FetchAvailableModelsResponse
		if err := json.Unmarshal(respBody, &payload); err != nil {
			return nil, fmt.Errorf("decode antigravity models: %w", err)
		}
		models := make([]string, 0, len(payload.Models))
		for model := range payload.Models {
			models = append(models, model)
		}
		return cleanLiveModelList(models), nil
	}
	if lastErr == nil {
		lastErr = errors.New("antigravity models lookup failed")
	}
	return nil, lastErr
}

func (s *GatewayService) liveGeminiOAuthModels(ctx context.Context, account *Account) ([]string, error) {
	if account == nil {
		return nil, errors.New("account is nil")
	}
	if s == nil || s.geminiTokenProvider == nil {
		return nil, errors.New("gemini token provider is not configured")
	}
	if account.IsGeminiCodeAssist() {
		return nil, nil
	}
	endpoint := liveGeminiModelEndpoint(account.GetGeminiBaseURL(geminicli.AIStudioBaseURL))
	validated, err := s.validateUpstreamBaseURL(endpoint)
	if err != nil {
		return nil, err
	}
	accessToken, err := s.geminiTokenProvider.GetAccessToken(ctx, account)
	if err != nil {
		return nil, err
	}
	body, _, err := s.doLiveModelsRequest(ctx, account, http.MethodGet, validated, func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer "+accessToken)
	})
	if err != nil {
		return nil, err
	}
	return cleanLiveModelList(decodeGeminiModelIDs(body)), nil
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
	case acc.IsGemini() && acc.Type == AccountTypeOAuth:
		if acc.IsGeminiCodeAssist() {
			slog.Debug("gateway.live_model_source_empty_endpoint_path",
				"path", "gemini_code_assist_early_return",
				"account_id", acc.ID,
				"account_name", acc.Name,
				"platform", acc.Platform,
				"account_type", acc.Type,
			)
			return nil, "", "", nil
		}
		models, err = s.liveGeminiOAuthModels(ctx, acc)
		endpoint = "gemini"
		capability = "generateContent"
	case acc.Platform == PlatformAntigravity:
		models, err = s.liveAntigravityModels(ctx, acc)
		endpoint = "antigravity"
		capability = "messages"
	default:
		slog.Warn("gateway.live_model_source_empty_endpoint_path",
			"path", "default_branch_no_loader_match",
			"account_id", acc.ID,
			"account_name", acc.Name,
			"platform", acc.Platform,
			"account_type", acc.Type,
		)
		return nil, "", "", nil
	}
	if err != nil {
		return nil, endpoint, capability, err
	}
	return cleanLiveModelList(models), endpoint, capability, nil
}

func (s *GatewayService) liveModelSourceForAccount(ctx context.Context, account Account) (LiveModelSource, error) {
	models, endpoint, capability, err := s.liveAccountModels(ctx, account)
	if err != nil {
		return LiveModelSource{}, err
	}
	catalog := publicizeLiveModelCatalog(&account, models)
	return LiveModelSource{
		Account:        cloneAccountForLiveSource(account),
		Endpoint:       endpoint,
		Capability:     capability,
		Models:         catalog.Models,
		UpstreamModels: catalog.UpstreamModels,
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

	ctx, cancel := context.WithTimeout(ctx, s.liveModelSourceLookupTimeout())
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

func resolveLiveUpstreamModelFromSource(source LiveModelSource, requestedModel string) (string, bool) {
	requestedModel = strings.TrimSpace(requestedModel)
	if strings.TrimSpace(source.Endpoint) == "" || requestedModel == "" {
		return requestedModel, false
	}
	for _, candidate := range requestedModelLookupCandidates("", requestedModel) {
		for publicModel, upstreamModel := range source.UpstreamModels {
			if strings.TrimSpace(upstreamModel) == "" {
				continue
			}
			if modelListContainsRequestedModel([]string{publicModel}, candidate) {
				return upstreamModel, true
			}
		}
		for _, publicModel := range source.Models {
			if modelListContainsRequestedModel([]string{publicModel}, candidate) {
				if upstreamModel, ok := source.UpstreamModels[publicModel]; ok && strings.TrimSpace(upstreamModel) != "" {
					return upstreamModel, true
				}
				return publicModel, true
			}
		}
	}
	return requestedModel, false
}

func (s *GatewayService) ResolveLiveUpstreamModel(ctx context.Context, account *Account, requestedModel string) (string, bool) {
	if s == nil || account == nil {
		return strings.TrimSpace(requestedModel), false
	}
	source, err := s.cachedLiveModelSourceForAccountWithLoader(ctx, *account, s.liveModelSourceForAccount)
	if err != nil {
		return strings.TrimSpace(requestedModel), false
	}
	return resolveLiveUpstreamModelFromSource(source, requestedModel)
}

func (s *GatewayService) cachedLiveModelSourceForAccountOnly(account Account) (LiveModelSource, bool) {
	if s == nil || s.modelsListCache == nil {
		return LiveModelSource{}, false
	}
	key := liveModelSourceCacheKey(account)
	cached, ok := s.modelsListCache.Get(key)
	if !ok {
		return LiveModelSource{}, false
	}
	source, ok := cached.(LiveModelSource)
	if !ok {
		s.modelsListCache.Delete(key)
		return LiveModelSource{}, false
	}
	return cloneLiveModelSource(source), true
}

func (s *GatewayService) ResolveCachedLiveUpstreamModel(account *Account, requestedModel string) (string, bool) {
	if s == nil || account == nil {
		return strings.TrimSpace(requestedModel), false
	}
	source, ok := s.cachedLiveModelSourceForAccountOnly(*account)
	if !ok {
		return strings.TrimSpace(requestedModel), false
	}
	return resolveLiveUpstreamModelFromSource(source, requestedModel)
}

func (s *GatewayService) ResolveUpstreamModelForAccount(ctx context.Context, account *Account, requestedModel string) (string, string) {
	requestedModel = strings.TrimSpace(requestedModel)
	if account == nil || requestedModel == "" {
		return requestedModel, ""
	}
	if mappedModel, matched := s.ResolveCachedLiveUpstreamModel(account, requestedModel); matched {
		return mappedModel, "live"
	}
	if account.Type == AccountTypeAPIKey {
		if mappedModel, matched := account.ResolveUpstreamModel(requestedModel); matched {
			return mappedModel, "account"
		}
	}
	if account.Platform == PlatformAnthropic && account.Type != AccountTypeAPIKey {
		normalized := claude.NormalizeModelID(requestedModel)
		if normalized != requestedModel {
			return normalized, "prefix"
		}
	}
	return requestedModel, ""
}

func (s *OpenAIGatewayService) ResolveLiveUpstreamModel(ctx context.Context, account *Account, requestedModel string) (string, bool) {
	if s == nil || account == nil {
		return strings.TrimSpace(requestedModel), false
	}
	source, err := s.cachedLiveModelSourceForAccount(ctx, *account)
	if err != nil {
		return strings.TrimSpace(requestedModel), false
	}
	return resolveLiveUpstreamModelFromSource(source, requestedModel)
}

func (s *OpenAIGatewayService) ResolveCachedLiveUpstreamModel(account *Account, requestedModel string) (string, bool) {
	if s == nil || account == nil || s.gatewayService == nil {
		return strings.TrimSpace(requestedModel), false
	}
	source, ok := s.gatewayService.cachedLiveModelSourceForAccountOnly(*account)
	if !ok {
		return strings.TrimSpace(requestedModel), false
	}
	return resolveLiveUpstreamModelFromSource(source, requestedModel)
}

func (s *OpenAIGatewayService) ResolveUpstreamModelForAccount(ctx context.Context, account *Account, requestedModel string, defaultMappedModel string) (string, string) {
	requestedModel = strings.TrimSpace(requestedModel)
	if account == nil || requestedModel == "" {
		return requestedModel, ""
	}
	if defaultMappedModel != "" && defaultMappedModel == requestedModel {
		return defaultMappedModel, "default"
	}
	if mappedModel, matched := s.ResolveCachedLiveUpstreamModel(account, requestedModel); matched {
		return mappedModel, "live"
	}
	if mappedModel, matched := account.ResolveUpstreamModel(requestedModel); matched {
		return mappedModel, "account"
	}
	return requestedModel, ""
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
	return cleanLiveModelList(models), nil
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
	return cleanLiveModelList(models), nil
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
		models := cleanLiveModelList(decodeChatGPTModelIDs([]byte(resp.String())))
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
		slog.Warn("openai.live_model_source_empty_endpoint_path",
			"path", "openai_default_branch_no_loader_match",
			"account_id", acc.ID,
			"account_name", acc.Name,
			"platform", acc.Platform,
			"account_type", acc.Type,
		)
		return LiveModelSource{Account: acc}, nil
	}
	if err != nil {
		return LiveModelSource{}, err
	}
	catalog := publicizeLiveModelCatalog(acc, models)
	return LiveModelSource{
		Account:        acc,
		Endpoint:       endpoint,
		Capability:     capability,
		Models:         catalog.Models,
		UpstreamModels: catalog.UpstreamModels,
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

	ctx, cancel := context.WithTimeout(ctx, s.liveModelCacheGatewayService().liveModelSourceLookupTimeout())
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
	return s.liveModelCacheGatewayService().cachedLiveModelSourceForAccountWithLoader(ctx, account, s.liveModelSourceForAccount)
}

func (s *OpenAIGatewayService) liveModelCacheGatewayService() *GatewayService {
	if s.gatewayService != nil {
		return s.gatewayService
	}
	s.liveModelCacheOnce.Do(func() {
		modelsListTTL := resolveModelsListCacheTTL(s.cfg)
		s.liveModelCacheGateway = &GatewayService{
			cfg:                s.cfg,
			modelsListCache:    gocache.New(modelsListTTL, time.Minute),
			modelsListCacheTTL: modelsListTTL,
		}
	})
	return s.liveModelCacheGateway
}
