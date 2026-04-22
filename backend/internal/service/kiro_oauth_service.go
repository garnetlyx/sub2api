package service

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

type KiroImportResult struct {
	AccessToken     string `json:"access_token"`
	RefreshToken    string `json:"refresh_token"`
	ProfileArn      string `json:"profile_arn"`
	Region          string `json:"region"`
	Idp             string `json:"idp"`
	ExpiresIn       int64  `json:"expires_in"`
	AvailableModels []kiro.ModelInfo `json:"available_models,omitempty"`
}

type KiroOAuthService struct {
	proxyRepo   ProxyRepository
	accountRepo AccountRepository
	sessions    *kiroOAuthSessionStore
}

func NewKiroOAuthService(proxyRepo ProxyRepository, accountRepo AccountRepository) *KiroOAuthService {
	return &KiroOAuthService{
		proxyRepo:   proxyRepo,
		accountRepo: accountRepo,
		sessions:    newKiroOAuthSessionStore(),
	}
}

type kiroOAuthSession struct {
	CodeVerifier string
	State        string
	Region       string
	Idp          string
	ProxyURL     string
	CreatedAt    time.Time
}

type kiroOAuthSessionStore struct {
	mu       sync.RWMutex
	sessions map[string]*kiroOAuthSession
	stopOnce sync.Once
	stopCh   chan struct{}
}

const kiroOAuthSessionTTL = 30 * time.Minute

func newKiroOAuthSessionStore() *kiroOAuthSessionStore {
	store := &kiroOAuthSessionStore{
		sessions: make(map[string]*kiroOAuthSession),
		stopCh:   make(chan struct{}),
	}
	go store.cleanup()
	return store
}

func (s *kiroOAuthSessionStore) Set(sessionID string, session *kiroOAuthSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[sessionID] = session
}

func (s *kiroOAuthSessionStore) Get(sessionID string) (*kiroOAuthSession, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	session, ok := s.sessions[sessionID]
	if !ok {
		return nil, false
	}
	if time.Since(session.CreatedAt) > kiroOAuthSessionTTL {
		return nil, false
	}
	return session, true
}

func (s *kiroOAuthSessionStore) Delete(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, sessionID)
}

func (s *kiroOAuthSessionStore) cleanup() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			s.mu.Lock()
			for id, session := range s.sessions {
				if time.Since(session.CreatedAt) > kiroOAuthSessionTTL {
					delete(s.sessions, id)
				}
			}
			s.mu.Unlock()
		}
	}
}

type KiroAuthURLResult struct {
	SessionID string `json:"session_id"`
	AuthURL   string `json:"auth_url"`
}

func (s *KiroOAuthService) GenerateAuthURL(ctx context.Context, region string, idp kiro.SocialProvider, proxyID *int64) (*KiroAuthURLResult, error) {
	region = defaultRegion(region)
	proxyURL, err := s.resolveProxyURL(ctx, proxyID)
	if err != nil {
		return nil, err
	}

	pkce, err := kiro.GeneratePKCE()
	if err != nil {
		return nil, infraerrors.Newf(http.StatusInternalServerError, "KIRO_PKCE_FAILED", "failed to generate PKCE: %v", err)
	}

	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/oauth/callback", kiro.DefaultAuthPort)
	authURL := kiro.BuildAuthURL(region, idp, redirectURI, pkce)

	sessionID, err := randomSessionID()
	if err != nil {
		return nil, infraerrors.Newf(http.StatusInternalServerError, "KIRO_SESSION_FAILED", "failed to create session: %v", err)
	}

	s.sessions.Set(sessionID, &kiroOAuthSession{
		CodeVerifier: pkce.CodeVerifier,
		State:        pkce.State,
		Region:       region,
		Idp:          string(idp),
		ProxyURL:     proxyURL,
		CreatedAt:    time.Now(),
	})

	return &KiroAuthURLResult{
		SessionID: sessionID,
		AuthURL:   authURL,
	}, nil
}

func (s *KiroOAuthService) ExchangeCode(ctx context.Context, sessionID string, code string, state string, proxyID *int64) (*KiroImportResult, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, infraerrors.New(http.StatusBadRequest, "KIRO_SESSION_REQUIRED", "session_id is required")
	}

	session, ok := s.sessions.Get(sessionID)
	if !ok {
		return nil, infraerrors.New(http.StatusBadRequest, "KIRO_SESSION_NOT_FOUND", "session not found or expired")
	}

	if strings.TrimSpace(state) != session.State {
		return nil, infraerrors.New(http.StatusBadRequest, "KIRO_STATE_MISMATCH", "state parameter does not match")
	}

	proxyURL := session.ProxyURL
	if proxyID != nil {
		resolved, err := s.resolveProxyURL(ctx, proxyID)
		if err != nil {
			return nil, err
		}
		proxyURL = resolved
	}

	httpClient, err := kiro.NewHTTPClient(proxyURL)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusBadRequest, "KIRO_PROXY_INVALID", "invalid proxy: %v", err)
	}

	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/oauth/callback", kiro.DefaultAuthPort)
	tokenResp, err := kiro.ExchangeCode(ctx, httpClient, session.Region, code, session.CodeVerifier, redirectURI)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusBadGateway, "KIRO_CODE_EXCHANGE_FAILED", "code exchange failed: %v", err)
	}

	detectedRegion := kiro.ExtractRegionFromProfileArn(tokenResp.ProfileArn)
	effectiveRegion := detectedRegion
	if effectiveRegion == "" {
		effectiveRegion = session.Region
	}

	models, _ := kiro.ListModels(ctx, httpClient, effectiveRegion, tokenResp.AccessToken)

	s.sessions.Delete(sessionID)

	return &KiroImportResult{
		AccessToken:     tokenResp.AccessToken,
		RefreshToken:    tokenResp.RefreshToken,
		ProfileArn:      tokenResp.ProfileArn,
		Region:          effectiveRegion,
		Idp:             session.Idp,
		ExpiresIn:       tokenResp.ExpiresIn,
		AvailableModels: models,
	}, nil
}

func (s *KiroOAuthService) ImportRefreshToken(ctx context.Context, refreshToken string, region string, proxyID *int64) (*KiroImportResult, error) {
	refreshToken = strings.TrimSpace(refreshToken)
	if refreshToken == "" {
		return nil, infraerrors.New(http.StatusBadRequest, "KIRO_REFRESH_TOKEN_REQUIRED", "refresh_token is required")
	}
	region = defaultRegion(region)

	proxyURL, err := s.resolveProxyURL(ctx, proxyID)
	if err != nil {
		return nil, err
	}

	httpClient, err := kiro.NewHTTPClient(proxyURL)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusBadRequest, "KIRO_PROXY_INVALID", "invalid proxy: %v", err)
	}

	tokenResp, err := kiro.RefreshSocialToken(ctx, httpClient, region, refreshToken)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusUnauthorized, "KIRO_REFRESH_FAILED", "refresh token validation failed: %v", err)
	}

	detectedRegion := kiro.ExtractRegionFromProfileArn(tokenResp.ProfileArn)
	effectiveRegion := detectedRegion
	if effectiveRegion == "" {
		effectiveRegion = region
	}

	models, _ := kiro.ListModels(ctx, httpClient, effectiveRegion, tokenResp.AccessToken)

	return &KiroImportResult{
		AccessToken:     tokenResp.AccessToken,
		RefreshToken:    tokenResp.RefreshToken,
		ProfileArn:      tokenResp.ProfileArn,
		Region:          effectiveRegion,
		ExpiresIn:       tokenResp.ExpiresIn,
		AvailableModels: models,
	}, nil
}

func (s *KiroOAuthService) RefreshByRefreshToken(ctx context.Context, account *Account, proxyID *int64) (*KiroImportResult, error) {
	refreshToken := strings.TrimSpace(account.GetCredential("refresh_token"))
	if refreshToken == "" {
		return nil, infraerrors.New(http.StatusBadRequest, "KIRO_NO_REFRESH_TOKEN", "no refresh_token stored for this account")
	}

	region := account.GetExtraString("region")
	if region == "" {
		region = kiro.DefaultRegion
	}

	resolvedProxyID := proxyID
	if account.ProxyID != nil {
		resolvedProxyID = account.ProxyID
	}

	return s.ImportRefreshToken(ctx, refreshToken, region, resolvedProxyID)
}

func (s *KiroOAuthService) FindByProfileArn(ctx context.Context, profileArn string) (*Account, error) {
	if s.accountRepo == nil || strings.TrimSpace(profileArn) == "" {
		return nil, nil
	}
	accounts, err := s.accountRepo.FindByExtraField(ctx, "profile_arn", profileArn)
	if err != nil {
		return nil, err
	}
	for i := range accounts {
		if accounts[i].Platform == PlatformKiro {
			return &accounts[i], nil
		}
	}
	return nil, nil
}

func (s *KiroOAuthService) BuildAccountCredentials(result *KiroImportResult) map[string]any {
	if result == nil {
		return nil
	}
	creds := map[string]any{
		"access_token":  result.AccessToken,
		"refresh_token": result.RefreshToken,
	}
	if !time.Now().Add(time.Duration(result.ExpiresIn) * time.Second).IsZero() {
		creds["expires_at"] = time.Now().Add(time.Duration(result.ExpiresIn) * time.Second).UTC().Format(time.RFC3339)
	}
	return creds
}

func (s *KiroOAuthService) BuildAccountExtra(result *KiroImportResult) map[string]any {
	if result == nil {
		return nil
	}
	extra := map[string]any{
		"region":      result.Region,
		"profile_arn": result.ProfileArn,
		"auth_type":   "social",
	}
	if result.Idp != "" {
		extra["idp"] = result.Idp
	}
	if len(result.AvailableModels) > 0 {
		modelIDs := make([]string, 0, len(result.AvailableModels))
		for _, m := range result.AvailableModels {
			modelIDs = append(modelIDs, m.ModelID)
		}
		extra["available_models"] = modelIDs
	}
	return extra
}

func (s *KiroOAuthService) resolveProxyURL(ctx context.Context, proxyID *int64) (string, error) {
	if proxyID == nil || s.proxyRepo == nil {
		return "", nil
	}
	proxy, err := s.proxyRepo.GetByID(ctx, *proxyID)
	if err != nil {
		return "", infraerrors.Newf(http.StatusBadRequest, "KIRO_PROXY_NOT_FOUND", "proxy not found: %v", err)
	}
	if proxy == nil {
		return "", nil
	}
	return proxy.URL(), nil
}

func KiroTokenCacheKey(account *Account) string {
	if account == nil {
		return "kiro:unknown"
	}
	arn := strings.TrimSpace(account.GetExtraString("profile_arn"))
	if arn != "" {
		return fmt.Sprintf("kiro:%s", arn)
	}
	return fmt.Sprintf("kiro:%d", account.ID)
}

func defaultRegion(region string) string {
	region = strings.TrimSpace(region)
	if region == "" {
		return kiro.DefaultRegion
	}
	return region
}
