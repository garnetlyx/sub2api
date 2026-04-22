package service

import (
	"context"
	"time"
)

type KiroTokenRefresher struct {
	kiroOAuthService *KiroOAuthService
}

func NewKiroTokenRefresher(kiroOAuthService *KiroOAuthService) *KiroTokenRefresher {
	return &KiroTokenRefresher{kiroOAuthService: kiroOAuthService}
}

func (r *KiroTokenRefresher) CacheKey(account *Account) string {
	return KiroTokenCacheKey(account)
}

func (r *KiroTokenRefresher) CanRefresh(account *Account) bool {
	return account.Platform == PlatformKiro &&
		account.Type == AccountTypeOAuth &&
		account.GetCredential("refresh_token") != ""
}

func (r *KiroTokenRefresher) NeedsRefresh(account *Account, refreshWindow time.Duration) bool {
	expiresAt := account.GetCredentialAsTime("expires_at")
	if expiresAt == nil {
		return false
	}
	return time.Until(*expiresAt) < refreshWindow
}

func (r *KiroTokenRefresher) Refresh(ctx context.Context, account *Account) (map[string]any, error) {
	result, err := r.kiroOAuthService.RefreshByRefreshToken(ctx, account, account.ProxyID)
	if err != nil {
		return nil, err
	}
	newCreds := r.kiroOAuthService.BuildAccountCredentials(result)
	return MergeCredentials(account.Credentials, newCreds), nil
}
