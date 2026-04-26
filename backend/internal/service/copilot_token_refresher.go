package service

import (
	"context"
	"log/slog"
	"time"
)

// CopilotTokenRefresher implements OAuthRefreshExecutor for GitHub Copilot OAuth accounts.
//
// Refresh is intentionally lazy: CanRefresh returns false when no refresh_token is stored,
// so existing accounts (authorized before refresh_token support was added) are silently
// skipped by TokenRefreshService with zero overhead.
type CopilotTokenRefresher struct {
	copilotOAuthService *CopilotOAuthService
	accountRepo         AccountRepository
}

func NewCopilotTokenRefresher(copilotOAuthService *CopilotOAuthService, accountRepo AccountRepository) *CopilotTokenRefresher {
	return &CopilotTokenRefresher{copilotOAuthService: copilotOAuthService, accountRepo: accountRepo}
}

func (r *CopilotTokenRefresher) CacheKey(account *Account) string {
	return CopilotTokenCacheKey(account)
}

func (r *CopilotTokenRefresher) CanRefresh(account *Account) bool {
	return account.Platform == PlatformCopilot &&
		account.Type == AccountTypeOAuth &&
		account.GetCredential("refresh_token") != ""
}

func (r *CopilotTokenRefresher) NeedsRefresh(account *Account, refreshWindow time.Duration) bool {
	expiresAt := account.GetCredentialAsTime("expires_at")
	if expiresAt == nil {
		return false
	}
	return time.Until(*expiresAt) < refreshWindow
}

func (r *CopilotTokenRefresher) Refresh(ctx context.Context, account *Account) (map[string]any, error) {
	result, err := r.copilotOAuthService.RefreshByRefreshToken(ctx, account, account.ProxyID)
	if err != nil {
		return nil, err
	}
	newCreds := r.copilotOAuthService.BuildAccountCredentials(result)

	if len(result.AvailableModels) > 0 && r.accountRepo != nil {
		if extraErr := r.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{"available_models": result.AvailableModels}); extraErr != nil {
			slog.Warn("copilot_token_refresh.available_models_update_failed",
				"account_id", account.ID,
				"error", extraErr,
			)
		} else {
			// Patch in-memory so postRefreshActions → schedulerCache.SetAccount propagates the fresh list.
			if account.Extra == nil {
				account.Extra = make(map[string]any)
			}
			account.Extra["available_models"] = result.AvailableModels
		}
	}

	return MergeCredentials(account.Credentials, newCreds), nil
}
