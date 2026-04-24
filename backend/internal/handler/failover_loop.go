package handler

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"go.uber.org/zap"
)

// TempUnscheduler 用于 HandleFailoverError 中同账号重试耗尽后的临时封禁。
// GatewayService 隐式实现此接口。
type TempUnscheduler interface {
	TempUnscheduleRetryableError(ctx context.Context, accountID int64, failoverErr *service.UpstreamFailoverError)
}

type compatibilityExclusionLoader interface {
	LoadCompatibilityExcludedPlatforms(ctx context.Context, groupID *int64, scopeKey string) (map[string]time.Time, error)
}

type compatibilityExclusionRecorder interface {
	RememberCompatibilityExclusion(ctx context.Context, groupID *int64, scopeKey string, platform string, failoverErr *service.UpstreamFailoverError) error
}

// FailoverAction 表示 failover 错误处理后的下一步动作
type FailoverAction int

const (
	// FailoverContinue 继续循环（同账号重试或切换账号，调用方统一 continue）
	FailoverContinue FailoverAction = iota
	// FailoverExhausted 切换次数耗尽（调用方应返回错误响应）
	FailoverExhausted
	// FailoverCanceled context 已取消（调用方应直接 return）
	FailoverCanceled
)

const (
	// maxSameAccountRetries 同账号重试次数上限（针对 RetryableOnSameAccount 错误）
	maxSameAccountRetries = 3
	// sameAccountRetryDelay 同账号重试间隔
	sameAccountRetryDelay = 500 * time.Millisecond
	// singleAccountBackoffDelay 单账号分组 503 退避重试固定延时。
	// Service 层在 SingleAccountRetry 模式下已做充分原地重试（最多 3 次、总等待 30s），
	// Handler 层只需短暂间隔后重新进入 Service 层即可。
	singleAccountBackoffDelay = 2 * time.Second
)

// FailoverState 跨循环迭代共享的 failover 状态
type FailoverState struct {
	SwitchCount           int
	MaxSwitches           int
	FailedAccountIDs      map[int64]struct{}
	ExcludedPlatforms     map[string]struct{}
	SameAccountRetryCount map[int64]int
	LastFailoverErr       *service.UpstreamFailoverError
	ForceCacheBilling     bool
	hasBoundSession       bool
}

// NewFailoverState 创建 failover 状态
func NewFailoverState(maxSwitches int, hasBoundSession bool) *FailoverState {
	return &FailoverState{
		MaxSwitches:           maxSwitches,
		FailedAccountIDs:      make(map[int64]struct{}),
		ExcludedPlatforms:     make(map[string]struct{}),
		SameAccountRetryCount: make(map[int64]int),
		hasBoundSession:       hasBoundSession,
	}
}

// HandleFailoverError 处理 UpstreamFailoverError，返回下一步动作。
// 包含：缓存计费判断、同账号重试、临时封禁、切换计数、Antigravity 延时。
func (s *FailoverState) HandleFailoverError(
	ctx context.Context,
	gatewayService TempUnscheduler,
	accountID int64,
	platform string,
	failoverErr *service.UpstreamFailoverError,
) FailoverAction {
	s.LastFailoverErr = failoverErr

	// 缓存计费判断
	if needForceCacheBilling(s.hasBoundSession, failoverErr) {
		s.ForceCacheBilling = true
	}

	// 同账号重试：对 RetryableOnSameAccount 的临时性错误，先在同一账号上重试
	if failoverErr.RetryableOnSameAccount && s.SameAccountRetryCount[accountID] < maxSameAccountRetries {
		s.SameAccountRetryCount[accountID]++
		logger.FromContext(ctx).Warn("gateway.failover_same_account_retry",
			zap.Int64("account_id", accountID),
			zap.Int("upstream_status", failoverErr.StatusCode),
			zap.Int("same_account_retry_count", s.SameAccountRetryCount[accountID]),
			zap.Int("same_account_retry_max", maxSameAccountRetries),
		)
		if !sleepWithContext(ctx, sameAccountRetryDelay) {
			return FailoverCanceled
		}
		return FailoverContinue
	}

	// 同账号重试用尽，执行临时封禁
	if failoverErr.RetryableOnSameAccount {
		gatewayService.TempUnscheduleRetryableError(ctx, accountID, failoverErr)
	}

	// 加入失败列表
	s.FailedAccountIDs[accountID] = struct{}{}
	if failoverErr != nil && failoverErr.Reason == service.UpstreamFailoverReasonCompatibilityMismatch {
		if normalizedPlatform := normalizeExcludedPlatform(platform); normalizedPlatform != "" {
			s.ExcludedPlatforms[normalizedPlatform] = struct{}{}
		}
	}

	// 检查是否耗尽
	if s.SwitchCount >= s.MaxSwitches {
		return FailoverExhausted
	}

	// 递增切换计数
	s.SwitchCount++
	logger.FromContext(ctx).Warn("gateway.failover_switch_account",
		zap.Int64("account_id", accountID),
		zap.String("platform", platform),
		zap.Int("upstream_status", failoverErr.StatusCode),
		zap.Int("switch_count", s.SwitchCount),
		zap.Int("max_switches", s.MaxSwitches),
		zap.Int("excluded_platform_count", len(s.ExcludedPlatforms)),
	)

	// Antigravity 平台换号线性递增延时
	if platform == service.PlatformAntigravity {
		delay := time.Duration(s.SwitchCount-1) * time.Second
		if !sleepWithContext(ctx, delay) {
			return FailoverCanceled
		}
	}

	return FailoverContinue
}

// HandleSelectionExhausted 处理选号失败（所有候选账号都在排除列表中）时的退避重试决策。
// 针对 Antigravity 单账号分组的 503 (MODEL_CAPACITY_EXHAUSTED) 场景：
// 清除排除列表、等待退避后重新选号。
//
// 返回 FailoverContinue 时，调用方应设置 SingleAccountRetry context 并 continue。
// 返回 FailoverExhausted 时，调用方应返回错误响应。
// 返回 FailoverCanceled 时，调用方应直接 return。
func (s *FailoverState) HandleSelectionExhausted(ctx context.Context) FailoverAction {
	if s.LastFailoverErr != nil &&
		s.LastFailoverErr.StatusCode == http.StatusServiceUnavailable &&
		s.SwitchCount <= s.MaxSwitches {

		logger.FromContext(ctx).Warn("gateway.failover_single_account_backoff",
			zap.Duration("backoff_delay", singleAccountBackoffDelay),
			zap.Int("switch_count", s.SwitchCount),
			zap.Int("max_switches", s.MaxSwitches),
		)
		if !sleepWithContext(ctx, singleAccountBackoffDelay) {
			return FailoverCanceled
		}
		logger.FromContext(ctx).Warn("gateway.failover_single_account_retry",
			zap.Int("switch_count", s.SwitchCount),
			zap.Int("max_switches", s.MaxSwitches),
		)
		s.FailedAccountIDs = make(map[int64]struct{})
		s.ExcludedPlatforms = make(map[string]struct{})
		return FailoverContinue
	}
	return FailoverExhausted
}

func normalizeExcludedPlatform(platform string) string {
	return strings.ToLower(strings.TrimSpace(platform))
}

func seedFailoverStateExcludedPlatforms(fs *FailoverState, cached map[string]time.Time) {
	if fs == nil || len(cached) == 0 {
		return
	}
	for platform := range cached {
		if normalized := normalizeExcludedPlatform(platform); normalized != "" {
			fs.ExcludedPlatforms[normalized] = struct{}{}
		}
	}
}

func loadCompatibilityExcludedPlatforms(
	ctx context.Context,
	reqLog *zap.Logger,
	gatewayService compatibilityExclusionLoader,
	groupID *int64,
	scopeKey string,
	fs *FailoverState,
	logEvent string,
) {
	if gatewayService == nil || fs == nil || strings.TrimSpace(scopeKey) == "" {
		return
	}
	cached, err := gatewayService.LoadCompatibilityExcludedPlatforms(ctx, groupID, scopeKey)
	if err != nil {
		if reqLog != nil {
			reqLog.Warn(logEvent+"_load_failed", zap.Error(err))
		}
		return
	}
	if len(cached) == 0 {
		return
	}
	seedFailoverStateExcludedPlatforms(fs, cached)
	if reqLog != nil {
		reqLog.Info(logEvent,
			zap.Int("excluded_platform_count", len(cached)),
			zap.Strings("platforms", compatibilityExcludedPlatformNames(cached)),
		)
	}
}

func recordCompatibilityExclusion(
	ctx context.Context,
	reqLog *zap.Logger,
	gatewayService compatibilityExclusionRecorder,
	groupID *int64,
	scopeKey string,
	platform string,
	failoverErr *service.UpstreamFailoverError,
	logEvent string,
) {
	if gatewayService == nil || failoverErr == nil || !failoverErr.IsCompatibilityMismatch() || strings.TrimSpace(scopeKey) == "" {
		return
	}
	if err := gatewayService.RememberCompatibilityExclusion(ctx, groupID, scopeKey, platform, failoverErr); err != nil && reqLog != nil {
		reqLog.Warn(logEvent, zap.Error(err), zap.String("platform", platform))
	}
}

func compatibilityExcludedPlatformNames(cached map[string]time.Time) []string {
	if len(cached) == 0 {
		return nil
	}
	names := make([]string, 0, len(cached))
	for platform := range cached {
		if normalized := normalizeExcludedPlatform(platform); normalized != "" {
			names = append(names, normalized)
		}
	}
	sort.Strings(names)
	return names
}

// needForceCacheBilling 判断 failover 时是否需要强制缓存计费。
// 粘性会话切换账号、或上游明确标记时，将 input_tokens 转为 cache_read 计费。
func needForceCacheBilling(hasBoundSession bool, failoverErr *service.UpstreamFailoverError) bool {
	return hasBoundSession || (failoverErr != nil && failoverErr.ForceCacheBilling)
}

// sleepWithContext 等待指定时长，返回 false 表示 context 已取消。
func sleepWithContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}
