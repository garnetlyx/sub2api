package service

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/copilot"
)

const (
	copilotTokenRefreshSkew = 2 * time.Minute
	copilotTokenLockTTL     = 30 * time.Second
)

type CopilotTokenProvider struct {
	tokenCache GeminiTokenCache
	proxyRepo  ProxyRepository
}

func NewCopilotTokenProvider(tokenCache GeminiTokenCache, proxyRepo ProxyRepository) *CopilotTokenProvider {
	return &CopilotTokenProvider{
		tokenCache: tokenCache,
		proxyRepo:  proxyRepo,
	}
}

func (p *CopilotTokenProvider) GetAccessToken(ctx context.Context, account *Account) (string, error) {
	if account == nil {
		return "", errors.New("account is nil")
	}
	if !account.IsCopilot() || account.Type != AccountTypeOAuth {
		return "", errors.New("not a copilot oauth account")
	}

	cacheKey := CopilotTokenCacheKey(account)
	if p.tokenCache != nil {
		if token, err := p.tokenCache.GetAccessToken(ctx, cacheKey); err == nil && strings.TrimSpace(token) != "" {
			return token, nil
		}
	}

	if p.tokenCache != nil {
		locked, err := p.tokenCache.AcquireRefreshLock(ctx, cacheKey, copilotTokenLockTTL)
		if err == nil && locked {
			defer func() { _ = p.tokenCache.ReleaseRefreshLock(ctx, cacheKey) }()
		} else if err == nil && !locked {
			time.Sleep(80 * time.Millisecond)
			if token, cacheErr := p.tokenCache.GetAccessToken(ctx, cacheKey); cacheErr == nil && strings.TrimSpace(token) != "" {
				return token, nil
			}
		}
	}

	httpClient, err := copilot.NewHTTPClient(p.proxyURLForAccount(ctx, account))
	if err != nil {
		return "", err
	}

	tokenInfo, err := copilot.ExchangeCopilotToken(ctx, httpClient, account.GetCredential("access_token"))
	if err != nil {
		return "", err
	}

	if p.tokenCache != nil {
		ttl := time.Until(tokenInfo.ExpiresAt) - copilotTokenRefreshSkew
		if ttl <= 0 {
			ttl = 5 * time.Minute
		}
		_ = p.tokenCache.SetAccessToken(ctx, cacheKey, tokenInfo.Token, ttl)
	}

	return tokenInfo.Token, nil
}

func (p *CopilotTokenProvider) proxyURLForAccount(ctx context.Context, account *Account) string {
	if p == nil || p.proxyRepo == nil || account == nil || account.ProxyID == nil {
		return ""
	}
	proxy, err := p.proxyRepo.GetByID(ctx, *account.ProxyID)
	if err != nil || proxy == nil {
		return ""
	}
	return proxy.URL()
}
