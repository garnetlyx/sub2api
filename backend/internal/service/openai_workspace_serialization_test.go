package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// newSameWorkspaceTestScheduler constructs a minimal defaultOpenAIAccountScheduler
// wired to an OpenAIGatewayService with the given config. Returns the scheduler
// and its service so tests can poke sync.Map state directly.
func newSameWorkspaceTestScheduler(t *testing.T, cfg *config.Config) (*defaultOpenAIAccountScheduler, *OpenAIGatewayService) {
	t.Helper()
	if cfg == nil {
		cfg = &config.Config{}
	}
	cfg.Gateway.OpenAIWS.SameWorkspaceSerializationEnabled = true
	cfg.Gateway.OpenAIWS.SameWorkspaceSerializationCooldownSeconds = 60
	cfg.Gateway.OpenAIWS.SameUserSerializationEnabled = false
	svc := &OpenAIGatewayService{cfg: cfg}
	sched := &defaultOpenAIAccountScheduler{service: svc}
	return sched, svc
}

func openAIOAuthAccount(id int64, userID, workspaceID string) *Account {
	creds := map[string]any{}
	if userID != "" {
		creds["chatgpt_user_id"] = userID
	}
	if workspaceID != "" {
		creds["chatgpt_account_id"] = workspaceID
	}
	return &Account{
		ID:          id,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Credentials: creds,
	}
}

// recordWorkspace manually seeds the scheduler's last-selected state for a
// workspace, simulating a prior selection without going through the full
// scheduling pipeline.
func recordWorkspace(svc *OpenAIGatewayService, workspaceID string, accountID int64, activeAt time.Time) {
	st := &sameWorkspaceScheduleState{accountID: accountID, activeAt: activeAt}
	svc.sameWorkspaceLastScheduled.Store(workspaceID, st)
}

func TestFilterSameWorkspaceSerialized_ExcludesOtherSeatsInSameWorkspaceWithinCooldown(t *testing.T) {
	sched, svc := newSameWorkspaceTestScheduler(t, nil)
	a1 := openAIOAuthAccount(1, "user-A", "ws-team")
	a2 := openAIOAuthAccount(2, "user-B", "ws-team")
	a3 := openAIOAuthAccount(3, "user-C", "ws-other")
	candidates := []*Account{a1, a2, a3}

	// Account 1 was active 10s ago — within 60s cooldown
	recordWorkspace(svc, "ws-team", 1, time.Now().Add(-10*time.Second))

	got := sched.filterSameWorkspaceSerialized(candidates)
	require.Len(t, got, 2, "ws-team member 2 should be excluded; a3 in different workspace stays")
	ids := []int64{got[0].ID, got[1].ID}
	require.ElementsMatch(t, []int64{1, 3}, ids)
}

func TestFilterSameWorkspaceSerialized_KeepsAllAccountsWhenCooldownExpired(t *testing.T) {
	sched, svc := newSameWorkspaceTestScheduler(t, nil)
	a1 := openAIOAuthAccount(1, "user-A", "ws-team")
	a2 := openAIOAuthAccount(2, "user-B", "ws-team")
	candidates := []*Account{a1, a2}

	// Account 1 was active 5 minutes ago — well past 60s cooldown
	recordWorkspace(svc, "ws-team", 1, time.Now().Add(-5*time.Minute))

	got := sched.filterSameWorkspaceSerialized(candidates)
	require.Len(t, got, 2)
}

func TestFilterSameWorkspaceSerialized_NoOpWhenNoWorkspaceID(t *testing.T) {
	sched, _ := newSameWorkspaceTestScheduler(t, nil)
	// Two accounts with no workspace_id — neither participates in workspace filter
	a1 := openAIOAuthAccount(1, "user-A", "")
	a2 := openAIOAuthAccount(2, "user-B", "")
	candidates := []*Account{a1, a2}

	got := sched.filterSameWorkspaceSerialized(candidates)
	require.Len(t, got, 2, "accounts without workspace_id should pass through")
}

func TestFilterSameWorkspaceSerialized_NoOpWhenWorkspaceHasSingleSeat(t *testing.T) {
	sched, svc := newSameWorkspaceTestScheduler(t, nil)
	a1 := openAIOAuthAccount(1, "user-A", "ws-solo")
	candidates := []*Account{a1}

	// Even with state recorded, single-seat workspace should not self-exclude
	recordWorkspace(svc, "ws-solo", 1, time.Now().Add(-5*time.Second))
	_ = svc

	got := sched.filterSameWorkspaceSerialized(candidates)
	require.Len(t, got, 1)
}

func TestFilterSameWorkspaceSerialized_DisabledReturnsAll(t *testing.T) {
	cfg := &config.Config{}
	cfg.Gateway.OpenAIWS.SameWorkspaceSerializationEnabled = false
	svc := &OpenAIGatewayService{cfg: cfg}
	sched := &defaultOpenAIAccountScheduler{service: svc}
	a1 := openAIOAuthAccount(1, "user-A", "ws-team")
	a2 := openAIOAuthAccount(2, "user-B", "ws-team")
	candidates := []*Account{a1, a2}

	got := sched.filterSameWorkspaceSerialized(candidates)
	require.Len(t, got, 2, "feature disabled → no filtering")
}

func TestIsSameWorkspaceExcluded_StickyBypass(t *testing.T) {
	sched, svc := newSameWorkspaceTestScheduler(t, nil)

	// Record account 1 as active in workspace ws-team
	recordWorkspace(svc, "ws-team", 1, time.Now().Add(-10*time.Second))

	// Account 2 in same workspace within cooldown → excluded
	a2 := openAIOAuthAccount(2, "user-B", "ws-team")
	require.True(t, sched.isSameWorkspaceExcluded(a2))

	// Account 1 itself → not excluded (it's the active one)
	a1 := openAIOAuthAccount(1, "user-A", "ws-team")
	require.False(t, sched.isSameWorkspaceExcluded(a1))

	// Account 3 in different workspace → not excluded
	a3 := openAIOAuthAccount(3, "user-C", "ws-other")
	require.False(t, sched.isSameWorkspaceExcluded(a3))
}

func TestRecordSameWorkspaceSelection_UpdatesState(t *testing.T) {
	sched, _ := newSameWorkspaceTestScheduler(t, nil)
	a1 := openAIOAuthAccount(1, "user-A", "ws-team")

	require.False(t, sched.isSameWorkspaceExcluded(a1), "no prior selection → not excluded")

	sched.recordSameWorkspaceSelection(a1)

	// Now a2 (same workspace) should be excluded, a1 should not be
	a2 := openAIOAuthAccount(2, "user-B", "ws-team")
	require.False(t, sched.isSameWorkspaceExcluded(a1))
	require.True(t, sched.isSameWorkspaceExcluded(a2))
}

// --- Refresh path tests ---

func TestRefreshLockKeys_NonOpenAIReturnsSingleKey(t *testing.T) {
	account := &Account{
		ID:       1,
		Platform: PlatformAnthropic,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"refresh_token": "rt",
		},
	}
	got := refreshLockKeys(account, "claude:account:1")
	require.Equal(t, []string{"claude:account:1"}, got)
}

func TestRefreshLockKeys_OpenAIWithoutWorkspaceReturnsSingleKey(t *testing.T) {
	account := openAIOAuthAccount(1, "user-A", "")
	got := refreshLockKeys(account, "openai:user:user-A")
	require.Equal(t, []string{"openai:user:user-A"}, got)
}

func TestRefreshLockKeys_OpenAIWithWorkspaceReturnsWorkspaceFirstThenUser(t *testing.T) {
	account := openAIOAuthAccount(1, "user-A", "ws-team")
	got := refreshLockKeys(account, "openai:user:user-A")
	require.Equal(t, []string{"openai:ws:ws-team", "openai:user:user-A"}, got,
		"workspace must come first so concurrent refreshes across users sharing a workspace acquire locks in consistent order")
}

// mockTokenCacheForLockOrder records the order of AcquireRefreshLock calls.
type mockTokenCacheForLockOrder struct {
	mu           sync.Mutex
	acquiredKeys []string
	releasedKeys []string
	failAcquire  map[string]bool // keys in this map return acquired=false
	acquireErr   map[string]error
}

func (m *mockTokenCacheForLockOrder) GetAccessToken(_ context.Context, _ string) (string, error) {
	return "", errors.New("not implemented")
}
func (m *mockTokenCacheForLockOrder) SetAccessToken(_ context.Context, _ string, _ string, _ time.Duration) error {
	return errors.New("not implemented")
}
func (m *mockTokenCacheForLockOrder) DeleteAccessToken(_ context.Context, _ string) error {
	return errors.New("not implemented")
}

func (m *mockTokenCacheForLockOrder) AcquireRefreshLock(_ context.Context, cacheKey string, _ time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.acquiredKeys = append(m.acquiredKeys, cacheKey)
	if err, ok := m.acquireErr[cacheKey]; ok {
		return false, err
	}
	if m.failAcquire[cacheKey] {
		return false, nil
	}
	return true, nil
}

func (m *mockTokenCacheForLockOrder) ReleaseRefreshLock(_ context.Context, cacheKey string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.releasedKeys = append(m.releasedKeys, cacheKey)
	return nil
}

func TestRefreshIfNeeded_OpenAIAcquiresBothLocksInOrder(t *testing.T) {
	cache := &mockTokenCacheForLockOrder{}
	repo := &oauthRefreshLockOrderTestRepo{}
	api := NewOAuthRefreshAPI(repo, cache)

	account := openAIOAuthAccount(1, "user-A", "ws-team")
	executor := &noopRefreshExecutor{
		cacheKey: OpenAIUserTokenCacheKey(account),
		refreshResult: map[string]any{
			"access_token":  "at-new",
			"refresh_token": "rt-new",
			"expires_at":    time.Now().Add(time.Hour).Format(time.RFC3339),
		},
	}

	_, err := api.RefreshIfNeeded(context.Background(), account, executor, time.Hour)
	require.NoError(t, err)

	require.Equal(t,
		[]string{"openai:ws:ws-team", "openai:user:user-A"},
		cache.acquiredKeys,
		"workspace lock must be acquired before user lock",
	)
	require.Equal(t,
		[]string{"openai:user:user-A", "openai:ws:ws-team"},
		cache.releasedKeys,
		"locks must be released in reverse order (user before workspace)",
	)
}

func TestRefreshIfNeeded_WorkspaceLockHeldReturnsLockHeld(t *testing.T) {
	cache := &mockTokenCacheForLockOrder{
		failAcquire: map[string]bool{"openai:ws:ws-team": true},
	}
	repo := &oauthRefreshLockOrderTestRepo{}
	api := NewOAuthRefreshAPI(repo, cache)

	account := openAIOAuthAccount(1, "user-A", "ws-team")
	executor := &noopRefreshExecutor{
		cacheKey: OpenAIUserTokenCacheKey(account),
		refreshCalled: atomic.Bool{},
	}

	result, err := api.RefreshIfNeeded(context.Background(), account, executor, time.Hour)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.LockHeld, "LockHeld must be true when workspace lock is contended")
	require.False(t, executor.refreshCalled.Load(), "Refresh must not be called when lock is held")

	// Only the workspace lock was attempted (failed before reaching user lock)
	require.Equal(t, []string{"openai:ws:ws-team"}, cache.acquiredKeys)
}

// TestRefreshIfNeeded_NoDeadlockBetweenSameWorkspaceAccounts spins two
// concurrent RefreshIfNeeded calls for accounts that share a workspace but
// have different user_ids. Both must complete without deadlock.
func TestRefreshIfNeeded_NoDeadlockBetweenSameWorkspaceAccounts(t *testing.T) {
	cache := &mockTokenCacheForLockOrder{}
	repo := &oauthRefreshLockOrderTestRepo{}
	api := NewOAuthRefreshAPI(repo, cache)

	account1 := openAIOAuthAccount(1, "user-A", "ws-team")
	account2 := openAIOAuthAccount(2, "user-B", "ws-team")

	executor1 := &noopRefreshExecutor{
		cacheKey:      OpenAIUserTokenCacheKey(account1),
		refreshResult: map[string]any{"access_token": "at-1", "refresh_token": "rt-1", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)},
	}
	executor2 := &noopRefreshExecutor{
		cacheKey:      OpenAIUserTokenCacheKey(account2),
		refreshResult: map[string]any{"access_token": "at-2", "refresh_token": "rt-2", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)},
	}

	done := make(chan error, 2)
	go func() { _, err := api.RefreshIfNeeded(context.Background(), account1, executor1, time.Hour); done <- err }()
	go func() { _, err := api.RefreshIfNeeded(context.Background(), account2, executor2, time.Hour); done <- err }()

	for i := 0; i < 2; i++ {
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Fatal("RefreshIfNeeded deadlocked between same-workspace accounts")
		}
	}
}

// --- helpers used only by this test file ---

type oauthRefreshLockOrderTestRepo struct {
	AccountRepository
	savedID    int64
	savedCreds map[string]any
}

func (r *oauthRefreshLockOrderTestRepo) GetByID(_ context.Context, id int64) (*Account, error) {
	return &Account{ID: id, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"refresh_token": "rt-old"}}, nil
}

func (r *oauthRefreshLockOrderTestRepo) UpdateCredentials(_ context.Context, id int64, creds map[string]any) error {
	r.savedID = id
	r.savedCreds = creds
	return nil
}

type noopRefreshExecutor struct {
	cacheKey      string
	refreshResult map[string]any
	refreshCalled atomic.Bool
}

func (e *noopRefreshExecutor) CanRefresh(*Account) bool                       { return true }
func (e *noopRefreshExecutor) NeedsRefresh(*Account, time.Duration) bool      { return true }
func (e *noopRefreshExecutor) CacheKey(*Account) string                       { return e.cacheKey }
func (e *noopRefreshExecutor) Refresh(_ context.Context, _ *Account) (map[string]any, error) {
	e.refreshCalled.Store(true)
	if e.refreshResult == nil {
		return nil, errors.New("refresh failed")
	}
	return e.refreshResult, nil
}

// guard against accidental removal of strings import in this file
var _ = strings.Contains
