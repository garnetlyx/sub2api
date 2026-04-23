package service

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
)

const (
	kiroTokenRefreshSkew = 5 * time.Minute
	kiroTokenLockTTL     = 30 * time.Second
)

type KiroTokenProvider struct {
	tokenCache GeminiTokenCache
	proxyRepo  ProxyRepository
}

func NewKiroTokenProvider(tokenCache GeminiTokenCache, proxyRepo ProxyRepository) *KiroTokenProvider {
	return &KiroTokenProvider{
		tokenCache: tokenCache,
		proxyRepo:  proxyRepo,
	}
}

func (p *KiroTokenProvider) GetAccessToken(ctx context.Context, account *Account) (string, error) {
	if account == nil {
		return "", errors.New("account is nil")
	}
	if !account.IsKiro() || account.Type != AccountTypeOAuth {
		return "", errors.New("not a kiro oauth account")
	}

	cacheKey := KiroTokenCacheKey(account)
	if p.tokenCache != nil {
		if token, err := p.tokenCache.GetAccessToken(ctx, cacheKey); err == nil && strings.TrimSpace(token) != "" {
			return token, nil
		}
	}

	if p.tokenCache != nil {
		locked, err := p.tokenCache.AcquireRefreshLock(ctx, cacheKey, kiroTokenLockTTL)
		if err == nil && locked {
			defer func() { _ = p.tokenCache.ReleaseRefreshLock(ctx, cacheKey) }()
		} else if err == nil && !locked {
			time.Sleep(80 * time.Millisecond)
			if token, cacheErr := p.tokenCache.GetAccessToken(ctx, cacheKey); cacheErr == nil && strings.TrimSpace(token) != "" {
				return token, nil
			}
		}
	}

	region := account.GetExtraString("region")
	if region == "" {
		region = kiro.DefaultRegion
	}

	httpClient, err := kiro.NewHTTPClient(p.proxyURLForAccount(ctx, account))
	if err != nil {
		return "", err
	}

	refreshToken := account.GetCredential("refresh_token")
	if refreshToken == "" {
		return "", errors.New("no refresh_token stored for kiro account")
	}

	var (
		accessToken string
		expiresIn   int64
	)
	if strings.EqualFold(account.GetExtraString("auth_type"), "device_code") {
		clientID := account.GetCredential("client_id")
		clientSecret := account.GetCredential("client_secret")
		tokenInfo, refreshErr := kiro.RefreshOIDCToken(ctx, httpClient, region, clientID, clientSecret, refreshToken)
		if refreshErr != nil {
			return "", refreshErr
		}
		accessToken = tokenInfo.AccessToken
		expiresIn = tokenInfo.ExpiresIn
	} else {
		tokenInfo, refreshErr := kiro.RefreshSocialToken(ctx, httpClient, region, refreshToken)
		if refreshErr != nil {
			return "", refreshErr
		}
		accessToken = tokenInfo.AccessToken
		expiresIn = tokenInfo.ExpiresIn
	}

	if p.tokenCache != nil {
		ttl := time.Duration(expiresIn)*time.Second - kiroTokenRefreshSkew
		if ttl <= 0 {
			ttl = 5 * time.Minute
		}
		_ = p.tokenCache.SetAccessToken(ctx, cacheKey, accessToken, ttl)
	}

	return accessToken, nil
}

func (p *KiroTokenProvider) proxyURLForAccount(ctx context.Context, account *Account) string {
	if p == nil || p.proxyRepo == nil || account == nil || account.ProxyID == nil {
		return ""
	}
	proxy, err := p.proxyRepo.GetByID(ctx, *account.ProxyID)
	if err != nil || proxy == nil {
		return ""
	}
	return proxy.URL()
}
