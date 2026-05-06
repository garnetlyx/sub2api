package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	gocache "github.com/patrickmn/go-cache"
	"github.com/stretchr/testify/require"
)

type userGroupRateRepoHotpathStub struct {
	UserGroupRateRepository

	rate  *float64
	err   error
	wait  <-chan struct{}
	calls atomic.Int64
}

func (s *userGroupRateRepoHotpathStub) GetByUserAndGroup(ctx context.Context, userID, groupID int64) (*float64, error) {
	s.calls.Add(1)
	if s.wait != nil {
		<-s.wait
	}
	if s.err != nil {
		return nil, s.err
	}
	return s.rate, nil
}

type usageLogWindowBatchRepoStub struct {
	UsageLogRepository

	batchResult map[int64]*usagestats.AccountStats
	batchErr    error
	batchCalls  atomic.Int64

	singleResult map[int64]*usagestats.AccountStats
	singleErr    error
	singleCalls  atomic.Int64
}

func (s *usageLogWindowBatchRepoStub) GetAccountWindowStatsBatch(ctx context.Context, accountIDs []int64, startTime time.Time) (map[int64]*usagestats.AccountStats, error) {
	s.batchCalls.Add(1)
	if s.batchErr != nil {
		return nil, s.batchErr
	}
	out := make(map[int64]*usagestats.AccountStats, len(accountIDs))
	for _, id := range accountIDs {
		if stats, ok := s.batchResult[id]; ok {
			out[id] = stats
		}
	}
	return out, nil
}

func (s *usageLogWindowBatchRepoStub) GetAccountWindowStats(ctx context.Context, accountID int64, startTime time.Time) (*usagestats.AccountStats, error) {
	s.singleCalls.Add(1)
	if s.singleErr != nil {
		return nil, s.singleErr
	}
	if stats, ok := s.singleResult[accountID]; ok {
		return stats, nil
	}
	return &usagestats.AccountStats{}, nil
}

type sessionLimitCacheHotpathStub struct {
	SessionLimitCache

	batchData map[int64]float64
	batchErr  error

	setData map[int64]float64
	setErr  error
}

func (s *sessionLimitCacheHotpathStub) GetWindowCostBatch(ctx context.Context, accountIDs []int64) (map[int64]float64, error) {
	if s.batchErr != nil {
		return nil, s.batchErr
	}
	out := make(map[int64]float64, len(accountIDs))
	for _, id := range accountIDs {
		if v, ok := s.batchData[id]; ok {
			out[id] = v
		}
	}
	return out, nil
}

func (s *sessionLimitCacheHotpathStub) SetWindowCost(ctx context.Context, accountID int64, cost float64) error {
	if s.setErr != nil {
		return s.setErr
	}
	if s.setData == nil {
		s.setData = make(map[int64]float64)
	}
	s.setData[accountID] = cost
	return nil
}

type modelsListAccountRepoStub struct {
	AccountRepository

	byGroup map[int64][]Account
	all     []Account
	err     error

	listByGroupCalls atomic.Int64
	listAllCalls     atomic.Int64
}

type liveModelsHTTPUpstreamStub struct {
	responses       []string
	responsesByHost map[string]string
	calls           atomic.Int64
}

func (s *liveModelsHTTPUpstreamStub) Do(req *http.Request, proxyURL string, accountID int64, accountConcurrency int) (*http.Response, error) {
	call := int(s.calls.Add(1))
	idx := call - 1
	if idx >= len(s.responses) {
		idx = len(s.responses) - 1
	}
	body := "{}"
	hostMatched := false
	if req != nil && req.URL != nil && s.responsesByHost != nil {
		if matched := strings.TrimSpace(s.responsesByHost[req.URL.Host]); matched != "" {
			body = matched
			hostMatched = true
		}
	}
	if !hostMatched && idx >= 0 && len(s.responses) > 0 {
		body = s.responses[idx]
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}, nil
}

func (s *liveModelsHTTPUpstreamStub) DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	return s.Do(req, proxyURL, accountID, accountConcurrency)
}

type stickyGatewayCacheHotpathStub struct {
	GatewayCache

	stickyID int64
	getCalls atomic.Int64
}

func (s *stickyGatewayCacheHotpathStub) GetSessionAccountID(ctx context.Context, groupID int64, sessionHash string) (int64, error) {
	s.getCalls.Add(1)
	if s.stickyID > 0 {
		return s.stickyID, nil
	}
	return 0, errors.New("not found")
}

func (s *stickyGatewayCacheHotpathStub) SetSessionAccountID(ctx context.Context, groupID int64, sessionHash string, accountID int64, ttl time.Duration) error {
	return nil
}

func (s *stickyGatewayCacheHotpathStub) RefreshSessionTTL(ctx context.Context, groupID int64, sessionHash string, ttl time.Duration) error {
	return nil
}

func (s *stickyGatewayCacheHotpathStub) DeleteSessionAccountID(ctx context.Context, groupID int64, sessionHash string) error {
	return nil
}

func (s *stickyGatewayCacheHotpathStub) GetCompatibilityExcludedPlatforms(ctx context.Context, groupID int64, scopeKey string) (map[string]time.Time, error) {
	return map[string]time.Time{}, nil
}

func (s *stickyGatewayCacheHotpathStub) SetCompatibilityExcludedPlatform(ctx context.Context, groupID int64, scopeKey string, platform string, observedAt time.Time, ttl time.Duration) error {
	return nil
}

func (s *modelsListAccountRepoStub) ListSchedulableByGroupID(ctx context.Context, groupID int64) ([]Account, error) {
	s.listByGroupCalls.Add(1)
	if s.err != nil {
		return nil, s.err
	}
	accounts, ok := s.byGroup[groupID]
	if !ok {
		return nil, nil
	}
	out := make([]Account, len(accounts))
	copy(out, accounts)
	return out, nil
}

func (s *modelsListAccountRepoStub) ListSchedulable(ctx context.Context) ([]Account, error) {
	s.listAllCalls.Add(1)
	if s.err != nil {
		return nil, s.err
	}
	out := make([]Account, len(s.all))
	copy(out, s.all)
	return out, nil
}

func (s *modelsListAccountRepoStub) GetByID(ctx context.Context, id int64) (*Account, error) {
	for _, accounts := range s.byGroup {
		for i := range accounts {
			if accounts[i].ID == id {
				account := accounts[i]
				return &account, nil
			}
		}
	}
	for i := range s.all {
		if s.all[i].ID == id {
			account := s.all[i]
			return &account, nil
		}
	}
	return nil, errors.New("account not found")
}

func resetGatewayHotpathStatsForTest() {
	windowCostPrefetchCacheHitTotal.Store(0)
	windowCostPrefetchCacheMissTotal.Store(0)
	windowCostPrefetchBatchSQLTotal.Store(0)
	windowCostPrefetchFallbackTotal.Store(0)
	windowCostPrefetchErrorTotal.Store(0)

	userGroupRateCacheHitTotal.Store(0)
	userGroupRateCacheMissTotal.Store(0)
	userGroupRateCacheLoadTotal.Store(0)
	userGroupRateCacheSFSharedTotal.Store(0)
	userGroupRateCacheFallbackTotal.Store(0)

	modelsListCacheHitTotal.Store(0)
	modelsListCacheMissTotal.Store(0)
	modelsListCacheStoreTotal.Store(0)
}

func TestGetUserGroupRateMultiplier_UsesCacheAndSingleflight(t *testing.T) {
	resetGatewayHotpathStatsForTest()

	rate := 1.7
	unblock := make(chan struct{})
	repo := &userGroupRateRepoHotpathStub{
		rate: &rate,
		wait: unblock,
	}
	svc := &GatewayService{
		userGroupRateRepo:  repo,
		userGroupRateCache: gocache.New(time.Minute, time.Minute),
		cfg: &config.Config{
			Gateway: config.GatewayConfig{
				UserGroupRateCacheTTLSeconds: 30,
			},
		},
	}

	const concurrent = 12
	results := make([]float64, concurrent)
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(concurrent)
	for i := 0; i < concurrent; i++ {
		go func(idx int) {
			defer wg.Done()
			<-start
			results[idx] = svc.getUserGroupRateMultiplier(context.Background(), 101, 202, 1.2)
		}(i)
	}

	close(start)
	time.Sleep(20 * time.Millisecond)
	close(unblock)
	wg.Wait()

	for _, got := range results {
		require.Equal(t, rate, got)
	}
	require.Equal(t, int64(1), repo.calls.Load())

	// 再次读取应命中缓存，不再回源。
	got := svc.getUserGroupRateMultiplier(context.Background(), 101, 202, 1.2)
	require.Equal(t, rate, got)
	require.Equal(t, int64(1), repo.calls.Load())

	hit, miss, load, sfShared, fallback := GatewayUserGroupRateCacheStats()
	require.GreaterOrEqual(t, hit, int64(1))
	require.Equal(t, int64(12), miss)
	require.Equal(t, int64(1), load)
	require.GreaterOrEqual(t, sfShared, int64(1))
	require.Equal(t, int64(0), fallback)
}

func TestGetUserGroupRateMultiplier_FallbackOnRepoError(t *testing.T) {
	resetGatewayHotpathStatsForTest()

	repo := &userGroupRateRepoHotpathStub{
		err: errors.New("db down"),
	}
	svc := &GatewayService{
		userGroupRateRepo:  repo,
		userGroupRateCache: gocache.New(time.Minute, time.Minute),
		cfg: &config.Config{
			Gateway: config.GatewayConfig{
				UserGroupRateCacheTTLSeconds: 30,
			},
		},
	}

	got := svc.getUserGroupRateMultiplier(context.Background(), 101, 202, 1.25)
	require.Equal(t, 1.25, got)
	require.Equal(t, int64(1), repo.calls.Load())

	_, _, _, _, fallback := GatewayUserGroupRateCacheStats()
	require.Equal(t, int64(1), fallback)
}

func TestGetUserGroupRateMultiplier_CacheHitAndNilRepo(t *testing.T) {
	resetGatewayHotpathStatsForTest()

	repo := &userGroupRateRepoHotpathStub{
		err: errors.New("should not be called"),
	}
	svc := &GatewayService{
		userGroupRateRepo:  repo,
		userGroupRateCache: gocache.New(time.Minute, time.Minute),
	}
	key := "101:202"
	svc.userGroupRateCache.Set(key, 2.3, time.Minute)

	got := svc.getUserGroupRateMultiplier(context.Background(), 101, 202, 1.1)
	require.Equal(t, 2.3, got)

	hit, miss, load, _, fallback := GatewayUserGroupRateCacheStats()
	require.Equal(t, int64(1), hit)
	require.Equal(t, int64(0), miss)
	require.Equal(t, int64(0), load)
	require.Equal(t, int64(0), fallback)
	require.Equal(t, int64(0), repo.calls.Load())

	// 无 repo 时直接返回分组默认倍率
	svc2 := &GatewayService{
		userGroupRateCache: gocache.New(time.Minute, time.Minute),
	}
	svc2.userGroupRateCache.Set(key, 1.9, time.Minute)
	require.Equal(t, 1.9, svc2.getUserGroupRateMultiplier(context.Background(), 101, 202, 1.4))
	require.Equal(t, 1.4, svc2.getUserGroupRateMultiplier(context.Background(), 0, 202, 1.4))
	svc2.userGroupRateCache.Delete(key)
	require.Equal(t, 1.4, svc2.getUserGroupRateMultiplier(context.Background(), 101, 202, 1.4))
}

func TestWithWindowCostPrefetch_BatchReadAndContextReuse(t *testing.T) {
	resetGatewayHotpathStatsForTest()

	windowStart := time.Now().Add(-30 * time.Minute).Truncate(time.Hour)
	windowEnd := windowStart.Add(5 * time.Hour)
	accounts := []Account{
		{
			ID:                 1,
			Platform:           PlatformAnthropic,
			Type:               AccountTypeOAuth,
			Extra:              map[string]any{"window_cost_limit": 100.0},
			SessionWindowStart: &windowStart,
			SessionWindowEnd:   &windowEnd,
		},
		{
			ID:                 2,
			Platform:           PlatformAnthropic,
			Type:               AccountTypeSetupToken,
			Extra:              map[string]any{"window_cost_limit": 100.0},
			SessionWindowStart: &windowStart,
			SessionWindowEnd:   &windowEnd,
		},
		{
			ID:       3,
			Platform: PlatformAnthropic,
			Type:     AccountTypeAPIKey,
			Extra:    map[string]any{"window_cost_limit": 100.0},
		},
	}

	cache := &sessionLimitCacheHotpathStub{
		batchData: map[int64]float64{
			1: 11.0,
		},
	}
	repo := &usageLogWindowBatchRepoStub{
		batchResult: map[int64]*usagestats.AccountStats{
			2: {StandardCost: 22.0},
		},
	}
	svc := &GatewayService{
		sessionLimitCache: cache,
		usageLogRepo:      repo,
	}

	outCtx := svc.withWindowCostPrefetch(context.Background(), accounts)
	require.NotNil(t, outCtx)

	cost1, ok1 := windowCostFromPrefetchContext(outCtx, 1)
	require.True(t, ok1)
	require.Equal(t, 11.0, cost1)

	cost2, ok2 := windowCostFromPrefetchContext(outCtx, 2)
	require.True(t, ok2)
	require.Equal(t, 22.0, cost2)

	_, ok3 := windowCostFromPrefetchContext(outCtx, 3)
	require.False(t, ok3)

	require.Equal(t, int64(1), repo.batchCalls.Load())
	require.Equal(t, 22.0, cache.setData[2])

	hit, miss, batchSQL, fallback, errCount := GatewayWindowCostPrefetchStats()
	require.Equal(t, int64(1), hit)
	require.Equal(t, int64(1), miss)
	require.Equal(t, int64(1), batchSQL)
	require.Equal(t, int64(0), fallback)
	require.Equal(t, int64(0), errCount)
}

func TestWithWindowCostPrefetch_AllHitNoSQL(t *testing.T) {
	resetGatewayHotpathStatsForTest()

	windowStart := time.Now().Add(-30 * time.Minute).Truncate(time.Hour)
	windowEnd := windowStart.Add(5 * time.Hour)
	accounts := []Account{
		{
			ID:                 1,
			Platform:           PlatformAnthropic,
			Type:               AccountTypeOAuth,
			Extra:              map[string]any{"window_cost_limit": 100.0},
			SessionWindowStart: &windowStart,
			SessionWindowEnd:   &windowEnd,
		},
		{
			ID:                 2,
			Platform:           PlatformAnthropic,
			Type:               AccountTypeSetupToken,
			Extra:              map[string]any{"window_cost_limit": 100.0},
			SessionWindowStart: &windowStart,
			SessionWindowEnd:   &windowEnd,
		},
	}

	cache := &sessionLimitCacheHotpathStub{
		batchData: map[int64]float64{
			1: 11.0,
			2: 22.0,
		},
	}
	repo := &usageLogWindowBatchRepoStub{}
	svc := &GatewayService{
		sessionLimitCache: cache,
		usageLogRepo:      repo,
	}

	outCtx := svc.withWindowCostPrefetch(context.Background(), accounts)
	cost1, ok1 := windowCostFromPrefetchContext(outCtx, 1)
	cost2, ok2 := windowCostFromPrefetchContext(outCtx, 2)
	require.True(t, ok1)
	require.True(t, ok2)
	require.Equal(t, 11.0, cost1)
	require.Equal(t, 22.0, cost2)
	require.Equal(t, int64(0), repo.batchCalls.Load())
	require.Equal(t, int64(0), repo.singleCalls.Load())

	hit, miss, batchSQL, fallback, errCount := GatewayWindowCostPrefetchStats()
	require.Equal(t, int64(2), hit)
	require.Equal(t, int64(0), miss)
	require.Equal(t, int64(0), batchSQL)
	require.Equal(t, int64(0), fallback)
	require.Equal(t, int64(0), errCount)
}

func TestWithWindowCostPrefetch_BatchErrorFallbackSingleQuery(t *testing.T) {
	resetGatewayHotpathStatsForTest()

	windowStart := time.Now().Add(-30 * time.Minute).Truncate(time.Hour)
	windowEnd := windowStart.Add(5 * time.Hour)
	accounts := []Account{
		{
			ID:                 2,
			Platform:           PlatformAnthropic,
			Type:               AccountTypeSetupToken,
			Extra:              map[string]any{"window_cost_limit": 100.0},
			SessionWindowStart: &windowStart,
			SessionWindowEnd:   &windowEnd,
		},
	}

	cache := &sessionLimitCacheHotpathStub{}
	repo := &usageLogWindowBatchRepoStub{
		batchErr: errors.New("batch failed"),
		singleResult: map[int64]*usagestats.AccountStats{
			2: {StandardCost: 33.0},
		},
	}
	svc := &GatewayService{
		sessionLimitCache: cache,
		usageLogRepo:      repo,
	}

	outCtx := svc.withWindowCostPrefetch(context.Background(), accounts)
	cost, ok := windowCostFromPrefetchContext(outCtx, 2)
	require.True(t, ok)
	require.Equal(t, 33.0, cost)
	require.Equal(t, int64(1), repo.batchCalls.Load())
	require.Equal(t, int64(1), repo.singleCalls.Load())

	_, _, _, fallback, errCount := GatewayWindowCostPrefetchStats()
	require.Equal(t, int64(1), fallback)
	require.Equal(t, int64(1), errCount)
}

func TestGetAvailableModels_UsesPerAccountLiveSourceCache(t *testing.T) {
	resetGatewayHotpathStatsForTest()

	groupID := int64(9)
	account := Account{
		ID:          1,
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": "https://upstream.test/v1",
		},
	}
	repo := &modelsListAccountRepoStub{
		byGroup: map[int64][]Account{
			groupID: {account},
		},
	}
	upstream := &liveModelsHTTPUpstreamStub{
		responses: []string{
			`{"data":[{"id":"claude-sonnet-4-6"},{"id":"claude-haiku-4-5"}]}`,
			`{"data":[{"id":"claude-opus-4-6"}]}`,
		},
	}
	svc := &GatewayService{
		accountRepo:        repo,
		httpUpstream:       upstream,
		cfg:                &config.Config{},
		modelsListCache:    gocache.New(time.Minute, time.Minute),
		modelsListCacheTTL: time.Minute,
	}

	models1 := svc.GetAvailableModels(context.Background(), &groupID, PlatformAnthropic)
	require.Equal(t, []string{"claude-haiku-4.5", "claude-sonnet-4.6"}, models1)
	require.Equal(t, int64(1), repo.listByGroupCalls.Load())

	models2 := svc.GetAvailableModels(context.Background(), &groupID, PlatformAnthropic)
	require.Equal(t, int64(2), repo.listByGroupCalls.Load())
	require.Equal(t, int64(1), upstream.calls.Load())
	require.Equal(t, models1, models2)

	svc.InvalidateLiveModelSourceCache(account.ID)
	models3 := svc.GetAvailableModels(context.Background(), &groupID, PlatformAnthropic)
	require.Equal(t, int64(2), upstream.calls.Load())
	require.Equal(t, []string{"claude-opus-4.6"}, models3)

	hit, miss, store := GatewayModelsListCacheStats()
	require.Equal(t, int64(1), hit)
	require.Equal(t, int64(2), miss)
	require.Equal(t, int64(2), store)
}

func TestLiveModelSourceCacheRefreshesWhenAccountSignatureChanges(t *testing.T) {
	resetGatewayHotpathStatsForTest()

	account := Account{
		ID:          7,
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": "https://upstream.test/v1",
		},
	}
	upstream := &liveModelsHTTPUpstreamStub{
		responses: []string{
			`{"data":[{"id":"claude-sonnet-4.6"}]}`,
			`{"data":[{"id":"claude-opus-4.7"}]}`,
		},
	}
	svc := &GatewayService{
		httpUpstream:       upstream,
		cfg:                &config.Config{},
		modelsListCache:    gocache.New(time.Minute, time.Minute),
		modelsListCacheTTL: time.Minute,
	}

	source1, err := svc.LiveModelSourceForAccount(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, []string{"claude-sonnet-4.6"}, source1.Models)

	account.Credentials["base_url"] = "https://upstream.test/v1/"
	source2, err := svc.LiveModelSourceForAccount(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, []string{"claude-opus-4.7"}, source2.Models)
	require.Equal(t, int64(2), upstream.calls.Load())
}

func TestLiveModelSourceCacheDoesNotStoreAccountsWithoutLiveEndpoint(t *testing.T) {
	resetGatewayHotpathStatsForTest()

	account := Account{
		ID:          8,
		Platform:    PlatformCopilot,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Schedulable: true,
	}
	svc := &GatewayService{
		modelsListCache:    gocache.New(time.Minute, time.Minute),
		modelsListCacheTTL: time.Minute,
	}
	var calls atomic.Int64

	source1, err := svc.cachedLiveModelSourceForAccountWithLoader(context.Background(), account, func(context.Context, Account) (LiveModelSource, error) {
		calls.Add(1)
		return LiveModelSource{Account: &account}, nil
	})
	require.NoError(t, err)
	require.Empty(t, source1.Endpoint)

	source2, err := svc.cachedLiveModelSourceForAccountWithLoader(context.Background(), account, func(context.Context, Account) (LiveModelSource, error) {
		calls.Add(1)
		return LiveModelSource{Account: &account, Endpoint: "copilot", Capability: "chat", Models: []string{"gpt-5.5"}}, nil
	})
	require.NoError(t, err)
	require.Equal(t, "copilot", source2.Endpoint)
	require.Equal(t, []string{"gpt-5.5"}, source2.Models)
	require.Equal(t, int64(2), calls.Load())

	source3, err := svc.cachedLiveModelSourceForAccountWithLoader(context.Background(), account, func(context.Context, Account) (LiveModelSource, error) {
		calls.Add(1)
		return LiveModelSource{Account: &account, Endpoint: "copilot", Capability: "chat", Models: []string{"gpt-5.6"}}, nil
	})
	require.NoError(t, err)
	require.Equal(t, []string{"gpt-5.5"}, source3.Models)
	require.Equal(t, int64(2), calls.Load())
}

func TestOpenAIGatewayLiveModelSourceUsesSharedAccountCache(t *testing.T) {
	resetGatewayHotpathStatsForTest()

	account := Account{
		ID:          11,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": "https://openai-compatible.test/v1",
		},
	}
	upstream := &liveModelsHTTPUpstreamStub{
		responses: []string{
			`{"data":[{"id":"gpt-5-5"},{"id":"minimax-m2-7"}]}`,
			`{"data":[{"id":"gpt-5.6"}]}`,
		},
	}
	gateway := &GatewayService{
		httpUpstream:       upstream,
		cfg:                &config.Config{},
		modelsListCache:    gocache.New(time.Minute, time.Minute),
		modelsListCacheTTL: time.Minute,
	}
	openaiGateway := &OpenAIGatewayService{
		httpUpstream:   upstream,
		gatewayService: gateway,
	}

	source1, err := openaiGateway.LiveModelSourceForAccount(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, []string{"gpt-5.5", "minimax-m2.7"}, source1.Models)

	source2, err := openaiGateway.LiveModelSourceForAccount(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, source1.Models, source2.Models)
	require.Equal(t, int64(1), upstream.calls.Load())

	gateway.InvalidateLiveModelSourceCache(account.ID)
	source3, err := openaiGateway.LiveModelSourceForAccount(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, []string{"gpt-5.6"}, source3.Models)
	require.Equal(t, int64(2), upstream.calls.Load())
}

func TestOpenAIGatewayLiveModelSourceUsesLocalAccountCacheWhenUnshared(t *testing.T) {
	resetGatewayHotpathStatsForTest()

	account := Account{
		ID:          12,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": "https://openai-compatible.test/v1",
		},
	}
	upstream := &liveModelsHTTPUpstreamStub{
		responses: []string{
			`{"data":[{"id":"gpt-5-5"}]}`,
			`{"data":[{"id":"gpt-5.6"}]}`,
		},
	}
	openaiGateway := &OpenAIGatewayService{
		cfg:          &config.Config{},
		httpUpstream: upstream,
	}

	source1, err := openaiGateway.LiveModelSourceForAccount(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, []string{"gpt-5.5"}, source1.Models)

	source2, err := openaiGateway.LiveModelSourceForAccount(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, source1.Models, source2.Models)
	require.Equal(t, int64(1), upstream.calls.Load())

	openaiGateway.liveModelCacheGatewayService().InvalidateLiveModelSourceCache(account.ID)
	source3, err := openaiGateway.LiveModelSourceForAccount(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, []string{"gpt-5.6"}, source3.Models)
	require.Equal(t, int64(2), upstream.calls.Load())
}

func TestGatewayLiveModelSourceDiscoversAntigravityModels(t *testing.T) {
	resetGatewayHotpathStatsForTest()

	account := Account{
		ID:          21,
		Platform:    PlatformAntigravity,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Schedulable: true,
		Credentials: map[string]any{
			"access_token": "ag-token",
			"project_id":   "project-123",
		},
	}
	repo := &modelsListAccountRepoStub{all: []Account{account}}
	upstream := &liveModelsHTTPUpstreamStub{
		responses: []string{`{"models":{"claude-sonnet-4-6":{},"gemini-3-1-pro-high":{}}}`},
	}
	svc := &GatewayService{
		accountRepo:              repo,
		httpUpstream:             upstream,
		cfg:                      &config.Config{},
		antigravityTokenProvider: NewAntigravityTokenProvider(repo, nil, nil),
		modelsListCache:          gocache.New(time.Minute, time.Minute),
		modelsListCacheTTL:       time.Minute,
	}

	source, err := svc.LiveModelSourceForAccount(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, "antigravity", source.Endpoint)
	require.Equal(t, []string{"claude-sonnet-4.6", "gemini-3.1-pro-high"}, source.Models)

	source2, err := svc.LiveModelSourceForAccount(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, source.Models, source2.Models)
	require.Equal(t, int64(1), upstream.calls.Load())
}

func TestGetAvailableModelsIncludesAntigravityLiveModels(t *testing.T) {
	resetGatewayHotpathStatsForTest()

	groupID := int64(42)
	account := Account{
		ID:          22,
		Platform:    PlatformAntigravity,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Schedulable: true,
		Credentials: map[string]any{
			"access_token": "ag-token",
			"project_id":   "project-123",
		},
	}
	repo := &modelsListAccountRepoStub{byGroup: map[int64][]Account{groupID: {account}}}
	upstream := &liveModelsHTTPUpstreamStub{
		responses: []string{`{"models":{"claude-opus-4-6":{},"claude-opus-4-6-thinking":{}}}`},
	}
	svc := &GatewayService{
		accountRepo:              repo,
		httpUpstream:             upstream,
		cfg:                      &config.Config{},
		antigravityTokenProvider: NewAntigravityTokenProvider(repo, nil, nil),
		modelsListCache:          gocache.New(time.Minute, time.Minute),
		modelsListCacheTTL:       time.Minute,
	}

	models := svc.GetAvailableModels(context.Background(), &groupID, PlatformAntigravity)
	require.Equal(t, []string{"claude-opus-4.6", "claude-opus-4.6-thinking"}, models)
	require.Equal(t, int64(1), upstream.calls.Load())
}

func TestGatewayModelSupportUsesAntigravityLiveModels(t *testing.T) {
	resetGatewayHotpathStatsForTest()

	account := Account{
		ID:          23,
		Platform:    PlatformAntigravity,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Schedulable: true,
		Credentials: map[string]any{
			"access_token": "ag-token",
			"project_id":   "project-123",
		},
	}
	repo := &modelsListAccountRepoStub{all: []Account{account}}
	upstream := &liveModelsHTTPUpstreamStub{
		responses: []string{`{"models":{"claude-sonnet-4-6":{}}}`},
	}
	svc := &GatewayService{
		accountRepo:              repo,
		httpUpstream:             upstream,
		cfg:                      &config.Config{},
		antigravityTokenProvider: NewAntigravityTokenProvider(repo, nil, nil),
		modelsListCache:          gocache.New(time.Minute, time.Minute),
		modelsListCacheTTL:       time.Minute,
	}

	require.True(t, svc.isModelSupportedByAccountWithContext(context.Background(), &account, "claude-sonnet-4.6"))
	require.False(t, svc.isModelSupportedByAccountWithContext(context.Background(), &account, "claude-opus-4.7"))
	require.Equal(t, int64(1), upstream.calls.Load())
}

func TestGetAvailableModels_AppliesKnownIssuePolicyAfterLiveSourceCache(t *testing.T) {
	resetGatewayHotpathStatsForTest()

	groupID := int64(9)
	repo := &modelsListAccountRepoStub{
		byGroup: map[int64][]Account{
			groupID: {
				{
					ID:          1,
					Platform:    PlatformAnthropic,
					Type:        AccountTypeAPIKey,
					Status:      StatusActive,
					Schedulable: true,
					Credentials: map[string]any{
						"api_key":  "sk-test",
						"base_url": "https://upstream.test/v1",
					},
				},
			},
		},
	}
	settings := &testSettingRepo{values: map[string]string{
		KnownIssueModelExclusionsSettingKey: `[{
			"model":"claude-opus-4-7",
			"platform":"anthropic",
			"evidence":"live provider returned invalid model for chat",
			"last_verified":"2026-05-06",
			"remove_when":"provider /models no longer lists the broken model"
		}]`,
	}}
	svc := &GatewayService{
		accountRepo: repo,
		httpUpstream: &liveModelsHTTPUpstreamStub{
			responses: []string{`{"data":[{"id":"claude-opus-4-7"},{"id":"claude-sonnet-4-6"}]}`},
		},
		cfg:                &config.Config{},
		modelsListCache:    gocache.New(time.Minute, time.Minute),
		modelsListCacheTTL: time.Minute,
		settingService:     NewSettingService(settings, &config.Config{}),
	}

	models1 := svc.GetAvailableModels(context.Background(), &groupID, PlatformAnthropic)
	require.Equal(t, []string{"claude-sonnet-4.6"}, models1)

	delete(settings.values, KnownIssueModelExclusionsSettingKey)
	models2 := svc.GetAvailableModels(context.Background(), &groupID, PlatformAnthropic)
	require.Equal(t, []string{"claude-opus-4.7", "claude-sonnet-4.6"}, models2)
	require.Equal(t, int64(1), svc.httpUpstream.(*liveModelsHTTPUpstreamStub).calls.Load())
}

func TestGetAvailableModels_ErrorAndGlobalListBranches(t *testing.T) {
	resetGatewayHotpathStatsForTest()

	errRepo := &modelsListAccountRepoStub{
		err: errors.New("db error"),
	}
	svcErr := &GatewayService{
		accountRepo: errRepo,
		cfg:         &config.Config{},
	}
	require.Empty(t, svcErr.GetAvailableModels(context.Background(), nil, ""))

	okRepo := &modelsListAccountRepoStub{
		all: []Account{
			{
				ID:          1,
				Platform:    PlatformAnthropic,
				Type:        AccountTypeAPIKey,
				Status:      StatusActive,
				Schedulable: true,
				Credentials: map[string]any{
					"api_key":  "sk-test",
					"base_url": "https://anthropic.test/v1",
				},
			},
			{
				ID:          2,
				Platform:    PlatformGemini,
				Type:        AccountTypeAPIKey,
				Status:      StatusActive,
				Schedulable: true,
				Priority:    2,
				Credentials: map[string]any{
					"api_key":  "gemini-key",
					"base_url": "https://gemini.test",
				},
			},
		},
	}
	svcOK := &GatewayService{
		accountRepo: okRepo,
		httpUpstream: &liveModelsHTTPUpstreamStub{responsesByHost: map[string]string{
			"anthropic.test": `{"data":[{"id":"claude-3-5-sonnet"}]}`,
			"gemini.test":    `{"models":[{"name":"models/gemini-2.5-pro"}]}`,
		}},
		cfg: &config.Config{},
	}
	models := svcOK.GetAvailableModels(context.Background(), nil, "")
	require.Equal(t, []string{"claude-3.5-sonnet", "gemini-2.5-pro"}, models)
	require.Equal(t, int64(1), okRepo.listAllCalls.Load())
}

func TestGetAvailableModels_CanonicalizesStyleAliasesAcrossProviders(t *testing.T) {
	resetGatewayHotpathStatsForTest()

	groupID := int64(77)
	repo := &modelsListAccountRepoStub{
		byGroup: map[int64][]Account{
			groupID: {
				{
					ID:          1,
					Platform:    PlatformAnthropic,
					Type:        AccountTypeAPIKey,
					Status:      StatusActive,
					Schedulable: true,
					Credentials: map[string]any{
						"api_key":  "sk-test",
						"base_url": "https://upstream.test/v1",
					},
				},
			},
		},
	}

	svc := &GatewayService{
		accountRepo:  repo,
		httpUpstream: &liveModelsHTTPUpstreamStub{responses: []string{`{"data":[{"id":"claude-sonnet-4.6"},{"id":"claude-sonnet-4-6"},{"id":"gpt-5.4"}]}`}},
		cfg:          &config.Config{},
	}

	models := svc.GetAvailableModels(context.Background(), &groupID, "")
	require.Equal(t, []string{"claude-sonnet-4.6", "gpt-5.4"}, models)
}

func TestPublicModelNumericVersionAliasesAreGeneric(t *testing.T) {
	tests := map[string]string{
		"gpt-5-5":                    "gpt-5.5",
		"openai/gpt-5-5":             "openai/gpt-5.5",
		"minimax-m2-7":               "minimax-m2.7",
		"glm-5-1":                    "glm-5.1",
		"deepseek-v3-2":              "deepseek-v3.2",
		"claude-sonnet-4-6":          "claude-sonnet-4.6",
		"claude-opus-4-7-thinking":   "claude-opus-4.7-thinking",
		"claude-3-5-sonnet-20241022": "claude-3-5-sonnet-20241022",
	}
	for input, expected := range tests {
		require.Equal(t, expected, CanonicalizePublicModel(input))
	}

	require.True(t, modelListContainsRequestedModel([]string{"minimax-m2-7"}, "minimax-m2.7"))
	require.True(t, modelListContainsRequestedModel([]string{"glm-5-1"}, "glm-5.1"))
	require.True(t, modelListContainsRequestedModel([]string{"deepseek-v3-2"}, "deepseek-v3.2"))
	require.True(t, modelListContainsRequestedModel([]string{"claude-sonnet-4-6"}, "claude-sonnet-4.6"))
	require.False(t, modelListContainsRequestedModel([]string{"claude-3-5-sonnet-20241022"}, "claude-3.5-sonnet-20241022"))
}

func TestGatewayHotpathHelpers_CacheTTLAndStickyContext(t *testing.T) {
	t.Run("resolve_user_group_rate_cache_ttl", func(t *testing.T) {
		require.Equal(t, defaultUserGroupRateCacheTTL, resolveUserGroupRateCacheTTL(nil))

		cfg := &config.Config{
			Gateway: config.GatewayConfig{
				UserGroupRateCacheTTLSeconds: 45,
			},
		}
		require.Equal(t, 45*time.Second, resolveUserGroupRateCacheTTL(cfg))
	})

	t.Run("resolve_models_list_cache_ttl", func(t *testing.T) {
		require.Equal(t, 60*time.Second, defaultModelsListCacheTTL)
		require.Equal(t, defaultModelsListCacheTTL, resolveModelsListCacheTTL(nil))

		cfg := &config.Config{
			Gateway: config.GatewayConfig{
				ModelsListCacheTTLSeconds: 20,
			},
		}
		require.Equal(t, 20*time.Second, resolveModelsListCacheTTL(cfg))
	})

	t.Run("prefetched_sticky_account_id_from_context", func(t *testing.T) {
		require.Equal(t, int64(0), prefetchedStickyAccountIDFromContext(context.TODO(), nil))
		require.Equal(t, int64(0), prefetchedStickyAccountIDFromContext(context.Background(), nil))

		ctx := context.WithValue(context.Background(), ctxkey.PrefetchedStickyAccountID, int64(123))
		ctx = context.WithValue(ctx, ctxkey.PrefetchedStickyGroupID, int64(0))
		require.Equal(t, int64(123), prefetchedStickyAccountIDFromContext(ctx, nil))

		groupID := int64(9)
		ctx2 := context.WithValue(context.Background(), ctxkey.PrefetchedStickyAccountID, 456)
		ctx2 = context.WithValue(ctx2, ctxkey.PrefetchedStickyGroupID, groupID)
		require.Equal(t, int64(456), prefetchedStickyAccountIDFromContext(ctx2, &groupID))

		ctx3 := context.WithValue(context.Background(), ctxkey.PrefetchedStickyAccountID, "invalid")
		ctx3 = context.WithValue(ctx3, ctxkey.PrefetchedStickyGroupID, groupID)
		require.Equal(t, int64(0), prefetchedStickyAccountIDFromContext(ctx3, &groupID))

		ctx4 := context.WithValue(context.Background(), ctxkey.PrefetchedStickyAccountID, int64(789))
		ctx4 = context.WithValue(ctx4, ctxkey.PrefetchedStickyGroupID, int64(10))
		require.Equal(t, int64(0), prefetchedStickyAccountIDFromContext(ctx4, &groupID))
	})

	t.Run("window_cost_from_prefetch_context", func(t *testing.T) {
		require.Equal(t, false, func() bool {
			_, ok := windowCostFromPrefetchContext(context.TODO(), 0)
			return ok
		}())
		require.Equal(t, false, func() bool {
			_, ok := windowCostFromPrefetchContext(context.Background(), 1)
			return ok
		}())

		ctx := context.WithValue(context.Background(), windowCostPrefetchContextKey, map[int64]float64{
			9: 12.34,
		})
		cost, ok := windowCostFromPrefetchContext(ctx, 9)
		require.True(t, ok)
		require.Equal(t, 12.34, cost)
	})
}

func TestInvalidateAvailableModelsCache_ByDimensions(t *testing.T) {
	svc := &GatewayService{
		modelsListCache: gocache.New(time.Minute, time.Minute),
	}
	group9 := int64(9)
	group10 := int64(10)
	svc.modelsListCache.Set(modelsListCacheKey(&group9, PlatformAnthropic), []string{"a"}, time.Minute)
	svc.modelsListCache.Set(modelsListCacheKey(&group9, PlatformGemini), []string{"b"}, time.Minute)
	svc.modelsListCache.Set(modelsListCacheKey(&group10, PlatformAnthropic), []string{"c"}, time.Minute)
	svc.modelsListCache.Set("invalid-key", []string{"d"}, time.Minute)

	t.Run("invalidate_group_and_platform", func(t *testing.T) {
		svc.InvalidateAvailableModelsCache(&group9, PlatformAnthropic)
		_, found := svc.modelsListCache.Get(modelsListCacheKey(&group9, PlatformAnthropic))
		require.False(t, found)
		_, stillFound := svc.modelsListCache.Get(modelsListCacheKey(&group9, PlatformGemini))
		require.True(t, stillFound)
	})

	t.Run("invalidate_group_only", func(t *testing.T) {
		svc.InvalidateAvailableModelsCache(&group9, "")
		_, foundA := svc.modelsListCache.Get(modelsListCacheKey(&group9, PlatformAnthropic))
		_, foundB := svc.modelsListCache.Get(modelsListCacheKey(&group9, PlatformGemini))
		require.False(t, foundA)
		require.False(t, foundB)
		_, foundOtherGroup := svc.modelsListCache.Get(modelsListCacheKey(&group10, PlatformAnthropic))
		require.True(t, foundOtherGroup)
	})

	t.Run("invalidate_platform_only", func(t *testing.T) {
		// 重建数据后仅按 platform 失效
		svc.modelsListCache.Set(modelsListCacheKey(&group9, PlatformAnthropic), []string{"a"}, time.Minute)
		svc.modelsListCache.Set(modelsListCacheKey(&group9, PlatformGemini), []string{"b"}, time.Minute)
		svc.modelsListCache.Set(modelsListCacheKey(&group10, PlatformAnthropic), []string{"c"}, time.Minute)

		svc.InvalidateAvailableModelsCache(nil, PlatformAnthropic)
		_, found9Anthropic := svc.modelsListCache.Get(modelsListCacheKey(&group9, PlatformAnthropic))
		_, found10Anthropic := svc.modelsListCache.Get(modelsListCacheKey(&group10, PlatformAnthropic))
		_, found9Gemini := svc.modelsListCache.Get(modelsListCacheKey(&group9, PlatformGemini))
		require.False(t, found9Anthropic)
		require.False(t, found10Anthropic)
		require.True(t, found9Gemini)
	})
}

func TestSelectAccountWithLoadAwareness_StickyReadReuse(t *testing.T) {
	now := time.Now().Add(-time.Minute)
	account := Account{
		ID:          88,
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 4,
		Priority:    1,
		LastUsedAt:  &now,
	}

	repo := stubOpenAIAccountRepo{accounts: []Account{account}}
	concurrency := NewConcurrencyService(stubConcurrencyCache{})

	cfg := &config.Config{
		RunMode: config.RunModeStandard,
		Gateway: config.GatewayConfig{
			Scheduling: config.GatewaySchedulingConfig{
				LoadBatchEnabled:         true,
				StickySessionMaxWaiting:  3,
				StickySessionWaitTimeout: time.Second,
				FallbackWaitTimeout:      time.Second,
				FallbackMaxWaiting:       10,
			},
		},
	}

	baseCtx := context.WithValue(context.Background(), ctxkey.ForcePlatform, PlatformAnthropic)

	t.Run("without_prefetch_reads_cache_once", func(t *testing.T) {
		cache := &stickyGatewayCacheHotpathStub{stickyID: account.ID}
		svc := &GatewayService{
			accountRepo:        repo,
			cache:              cache,
			cfg:                cfg,
			concurrencyService: concurrency,
			userGroupRateCache: gocache.New(time.Minute, time.Minute),
			modelsListCache:    gocache.New(time.Minute, time.Minute),
			modelsListCacheTTL: time.Minute,
		}

		result, err := svc.SelectAccountWithLoadAwareness(baseCtx, nil, "sess-hash", "", nil, "", int64(0))
		require.NoError(t, err)
		require.NotNil(t, result)
		require.NotNil(t, result.Account)
		require.Equal(t, account.ID, result.Account.ID)
		require.Equal(t, int64(1), cache.getCalls.Load())
	})

	t.Run("with_prefetch_skips_cache_read", func(t *testing.T) {
		cache := &stickyGatewayCacheHotpathStub{stickyID: account.ID}
		svc := &GatewayService{
			accountRepo:        repo,
			cache:              cache,
			cfg:                cfg,
			concurrencyService: concurrency,
			userGroupRateCache: gocache.New(time.Minute, time.Minute),
			modelsListCache:    gocache.New(time.Minute, time.Minute),
			modelsListCacheTTL: time.Minute,
		}

		ctx := context.WithValue(baseCtx, ctxkey.PrefetchedStickyAccountID, account.ID)
		ctx = context.WithValue(ctx, ctxkey.PrefetchedStickyGroupID, int64(0))
		result, err := svc.SelectAccountWithLoadAwareness(ctx, nil, "sess-hash", "", nil, "", int64(0))
		require.NoError(t, err)
		require.NotNil(t, result)
		require.NotNil(t, result.Account)
		require.Equal(t, account.ID, result.Account.ID)
		require.Equal(t, int64(0), cache.getCalls.Load())
	})

	t.Run("with_prefetch_group_mismatch_reads_cache", func(t *testing.T) {
		cache := &stickyGatewayCacheHotpathStub{stickyID: account.ID}
		svc := &GatewayService{
			accountRepo:        repo,
			cache:              cache,
			cfg:                cfg,
			concurrencyService: concurrency,
			userGroupRateCache: gocache.New(time.Minute, time.Minute),
			modelsListCache:    gocache.New(time.Minute, time.Minute),
			modelsListCacheTTL: time.Minute,
		}

		ctx := context.WithValue(baseCtx, ctxkey.PrefetchedStickyAccountID, int64(999))
		ctx = context.WithValue(ctx, ctxkey.PrefetchedStickyGroupID, int64(77))
		result, err := svc.SelectAccountWithLoadAwareness(ctx, nil, "sess-hash", "", nil, "", int64(0))
		require.NoError(t, err)
		require.NotNil(t, result)
		require.NotNil(t, result.Account)
		require.Equal(t, account.ID, result.Account.ID)
		require.Equal(t, int64(1), cache.getCalls.Load())
	})
}
