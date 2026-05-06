package service

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
)

type KiroImportResult struct {
	AccessToken           string           `json:"access_token"`
	RefreshToken          string           `json:"refresh_token"`
	ClientID              string           `json:"client_id,omitempty"`
	ClientSecret          string           `json:"client_secret,omitempty"`
	ClientSecretExpiresAt time.Time        `json:"client_secret_expires_at,omitempty"`
	ProfileArn            string           `json:"profile_arn"`
	Region                string           `json:"region"`
	Idp                   string           `json:"idp"`
	Subject               string           `json:"subject,omitempty"`
	AuthType              string           `json:"auth_type"`
	ExpiresIn             int64            `json:"expires_in"`
	AvailableModels       []kiro.ModelInfo `json:"available_models,omitempty"`
}

type KiroOAuthService struct {
	proxyRepo   ProxyRepository
	accountRepo AccountRepository
	sessions    *kiroOAuthSessionStore
}

type KiroDeviceFlowStartResult struct {
	SessionID               string `json:"session_id"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete,omitempty"`
	ExpiresIn               int64  `json:"expires_in"`
	Interval                int64  `json:"interval"`
}

func NewKiroOAuthService(proxyRepo ProxyRepository, accountRepo AccountRepository) *KiroOAuthService {
	return &KiroOAuthService{
		proxyRepo:   proxyRepo,
		accountRepo: accountRepo,
		sessions:    newKiroOAuthSessionStore(),
	}
}

type kiroOAuthSession struct {
	CodeVerifier          string
	State                 string
	Region                string
	Idp                   string
	AuthType              string
	ClientID              string
	ClientSecret          string
	ClientSecretExpiresAt int64
	DeviceCode            string
	ProxyURL              string
	CreatedAt             time.Time
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

	redirectURI := kiro.DefaultRedirectURI
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
		AuthType:     "social",
		ProxyURL:     proxyURL,
		CreatedAt:    time.Now(),
	})

	return &KiroAuthURLResult{
		SessionID: sessionID,
		AuthURL:   authURL,
	}, nil
}

func (s *KiroOAuthService) StartDeviceFlow(ctx context.Context, region string, proxyID *int64) (*KiroDeviceFlowStartResult, error) {
	region = defaultRegion(region)
	proxyURL, err := s.resolveProxyURL(ctx, proxyID)
	if err != nil {
		return nil, err
	}
	httpClient, err := kiro.NewHTTPClient(proxyURL)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusBadRequest, "KIRO_PROXY_INVALID", "invalid proxy: %v", err)
	}
	clientReg, err := kiro.RegisterOIDCClient(ctx, httpClient, region, "Sub2API Kiro")
	if err != nil {
		return nil, infraerrors.Newf(http.StatusBadGateway, "KIRO_DEVICE_CLIENT_REGISTER_FAILED", "oidc client registration failed: %v", err)
	}
	deviceResp, err := kiro.StartDeviceAuthorization(ctx, httpClient, region, clientReg.ClientID, clientReg.ClientSecret)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusBadGateway, "KIRO_DEVICE_CODE_START_FAILED", "failed to start kiro device flow: %v", err)
	}
	sessionID, err := randomSessionID()
	if err != nil {
		return nil, infraerrors.Newf(http.StatusInternalServerError, "KIRO_DEVICE_SESSION_FAILED", "failed to create device session: %v", err)
	}
	s.sessions.Set(sessionID, &kiroOAuthSession{
		AuthType:              "device_code",
		Region:                region,
		ClientID:              clientReg.ClientID,
		ClientSecret:          clientReg.ClientSecret,
		ClientSecretExpiresAt: clientReg.ClientSecretExpiresAt,
		DeviceCode:            deviceResp.DeviceCode,
		ProxyURL:              proxyURL,
		CreatedAt:             time.Now(),
	})
	return &KiroDeviceFlowStartResult{
		SessionID:               sessionID,
		UserCode:                deviceResp.UserCode,
		VerificationURI:         deviceResp.VerificationURI,
		VerificationURIComplete: deviceResp.VerificationURIComplete,
		ExpiresIn:               deviceResp.ExpiresIn,
		Interval:                deviceResp.Interval,
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

	redirectURI := kiro.DefaultRedirectURI
	tokenResp, err := kiro.ExchangeCode(ctx, httpClient, session.Region, code, session.CodeVerifier, redirectURI)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusBadGateway, "KIRO_CODE_EXCHANGE_FAILED", "code exchange failed: %v", err)
	}

	detectedRegion := kiro.ExtractRegionFromProfileArn(tokenResp.ProfileArn)
	effectiveRegion := detectedRegion
	if effectiveRegion == "" {
		effectiveRegion = session.Region
	}

	models, _ := kiro.ListModels(ctx, httpClient, effectiveRegion, tokenResp.AccessToken, tokenResp.ProfileArn)

	s.sessions.Delete(sessionID)

	return &KiroImportResult{
		AccessToken:     tokenResp.AccessToken,
		RefreshToken:    tokenResp.RefreshToken,
		ProfileArn:      tokenResp.ProfileArn,
		Region:          effectiveRegion,
		Idp:             session.Idp,
		AuthType:        "social",
		ExpiresIn:       tokenResp.ExpiresIn,
		AvailableModels: models,
	}, nil
}

func (s *KiroOAuthService) PollDeviceFlow(ctx context.Context, sessionID string, proxyID *int64) (*KiroImportResult, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, infraerrors.New(http.StatusBadRequest, "KIRO_DEVICE_SESSION_REQUIRED", "session_id is required")
	}
	session, ok := s.sessions.Get(sessionID)
	if !ok || session.AuthType != "device_code" {
		return nil, infraerrors.New(http.StatusBadRequest, "KIRO_DEVICE_SESSION_NOT_FOUND", "device session not found or expired")
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
	tokenResp, err := kiro.PollDeviceAuthorization(ctx, httpClient, session.Region, session.ClientID, session.ClientSecret, session.DeviceCode)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusBadGateway, "KIRO_DEVICE_CODE_POLL_FAILED", "failed to poll kiro device flow: %v", err)
	}
	if tokenResp == nil {
		return nil, infraerrors.New(http.StatusBadGateway, "KIRO_DEVICE_CODE_EMPTY", "device flow returned empty response")
	}
	switch strings.TrimSpace(tokenResp.Error) {
	case "":
	case "authorization_pending":
		return nil, infraerrors.New(http.StatusConflict, "KIRO_DEVICE_AUTH_PENDING", "authorization is still pending")
	case "slow_down":
		return nil, infraerrors.New(http.StatusTooManyRequests, "KIRO_DEVICE_AUTH_SLOW_DOWN", "authorization is pending, please wait a few seconds before retrying")
	case "expired_token":
		s.sessions.Delete(sessionID)
		return nil, infraerrors.New(http.StatusBadRequest, "KIRO_DEVICE_AUTH_EXPIRED", "device code has expired, please restart authorization")
	case "access_denied":
		s.sessions.Delete(sessionID)
		return nil, infraerrors.New(http.StatusUnauthorized, "KIRO_DEVICE_AUTH_DENIED", "device code authorization was denied")
	default:
		return nil, infraerrors.Newf(http.StatusBadGateway, "KIRO_DEVICE_AUTH_FAILED", "device code authorization failed: %s", tokenResp.ErrorDescription)
	}
	subject := kiro.ExtractSubjectFromAccessToken(tokenResp.AccessToken)
	models, _ := kiro.ListModels(ctx, httpClient, session.Region, tokenResp.AccessToken, "")
	s.sessions.Delete(sessionID)
	return &KiroImportResult{
		AccessToken:           tokenResp.AccessToken,
		RefreshToken:          tokenResp.RefreshToken,
		ClientID:              session.ClientID,
		ClientSecret:          session.ClientSecret,
		ClientSecretExpiresAt: unixMillisOrSecondsToTime(session.ClientSecretExpiresAt),
		Region:                session.Region,
		Subject:               subject,
		AuthType:              "device_code",
		ExpiresIn:             tokenResp.ExpiresIn,
		AvailableModels:       models,
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

	models, _ := kiro.ListModels(ctx, httpClient, effectiveRegion, tokenResp.AccessToken, tokenResp.ProfileArn)

	return &KiroImportResult{
		AccessToken:     tokenResp.AccessToken,
		RefreshToken:    tokenResp.RefreshToken,
		ProfileArn:      tokenResp.ProfileArn,
		Region:          effectiveRegion,
		AuthType:        "social",
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

	if strings.EqualFold(account.GetExtraString("auth_type"), "device_code") {
		return s.RefreshDeviceAccount(ctx, account, resolvedProxyID)
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

func (s *KiroOAuthService) FindBySubject(ctx context.Context, subject string) (*Account, error) {
	if s.accountRepo == nil || strings.TrimSpace(subject) == "" {
		return nil, nil
	}
	accounts, err := s.accountRepo.FindByExtraField(ctx, "subject", subject)
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
	if result.ClientID != "" {
		creds["client_id"] = result.ClientID
	}
	if result.ClientSecret != "" {
		creds["client_secret"] = result.ClientSecret
	}
	if !result.ClientSecretExpiresAt.IsZero() {
		creds["client_secret_expires_at"] = result.ClientSecretExpiresAt.UTC().Format(time.RFC3339)
	}
	if !time.Now().Add(time.Duration(result.ExpiresIn) * time.Second).IsZero() {
		creds["expires_at"] = time.Now().Add(time.Duration(result.ExpiresIn) * time.Second).UTC().Format(time.RFC3339)
	}
	if mapping := buildKiroModelMapping(result.AvailableModels); len(mapping) > 0 {
		creds["model_mapping"] = mapping
	}
	return creds
}

func (s *KiroOAuthService) BuildAccountExtra(result *KiroImportResult) map[string]any {
	if result == nil {
		return nil
	}
	extra := map[string]any{
		"region":    result.Region,
		"auth_type": coalesceTrimmed(result.AuthType, "social"),
	}
	if result.ProfileArn != "" {
		extra["profile_arn"] = result.ProfileArn
	}
	if result.Idp != "" {
		extra["idp"] = result.Idp
	}
	if result.Subject != "" {
		extra["subject"] = result.Subject
	}
	if strings.EqualFold(result.AuthType, "device_code") {
		extra["oidc_issuer"] = "https://view.awsapps.com/start"
	}
	if len(result.AvailableModels) > 0 {
		modelIDs := make([]string, 0, len(result.AvailableModels))
		for _, m := range result.AvailableModels {
			modelID := kiro.NormalizeKiroModelID(m.ModelID)
			if strings.TrimSpace(modelID) != "" {
				modelIDs = append(modelIDs, modelID)
			}
		}
		extra["available_models"] = modelIDs
	}
	return extra
}

func buildKiroModelMapping(models []kiro.ModelInfo) map[string]any {
	if len(models) == 0 {
		return nil
	}
	mapping := make(map[string]any, len(models))
	for _, model := range models {
		raw := strings.TrimSpace(model.ModelID)
		if raw == "" {
			continue
		}
		public := kiro.NormalizeKiroModelID(raw)
		if public == "" {
			continue
		}
		mapping[public] = raw
	}
	if len(mapping) == 0 {
		return nil
	}
	return mapping
}

func (s *KiroOAuthService) RefreshDeviceAccount(ctx context.Context, account *Account, proxyID *int64) (*KiroImportResult, error) {
	refreshToken := strings.TrimSpace(account.GetCredential("refresh_token"))
	clientID := strings.TrimSpace(account.GetCredential("client_id"))
	clientSecret := strings.TrimSpace(account.GetCredential("client_secret"))
	if refreshToken == "" {
		return nil, infraerrors.New(http.StatusBadRequest, "KIRO_NO_REFRESH_TOKEN", "no refresh_token stored for this account")
	}
	if clientID == "" || clientSecret == "" {
		return nil, infraerrors.New(http.StatusBadRequest, "KIRO_DEVICE_CLIENT_REQUIRED", "device code account is missing client credentials")
	}
	region := account.GetExtraString("region")
	if region == "" {
		region = kiro.DefaultRegion
	}
	proxyURL, err := s.resolveProxyURL(ctx, proxyID)
	if err != nil {
		return nil, err
	}
	httpClient, err := kiro.NewHTTPClient(proxyURL)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusBadRequest, "KIRO_PROXY_INVALID", "invalid proxy: %v", err)
	}
	tokenResp, err := kiro.RefreshOIDCToken(ctx, httpClient, region, clientID, clientSecret, refreshToken)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusUnauthorized, "KIRO_OIDC_REFRESH_FAILED", "oidc refresh failed: %v", err)
	}
	models, _ := kiro.ListModels(ctx, httpClient, region, tokenResp.AccessToken, account.GetExtraString("profile_arn"))
	return &KiroImportResult{
		AccessToken:           tokenResp.AccessToken,
		RefreshToken:          coalesceTrimmed(tokenResp.RefreshToken, refreshToken),
		ClientID:              clientID,
		ClientSecret:          clientSecret,
		ClientSecretExpiresAt: account.GetCredentialAsTimeValue("client_secret_expires_at"),
		Region:                region,
		Subject:               coalesceTrimmed(kiro.ExtractSubjectFromAccessToken(tokenResp.AccessToken), account.GetExtraString("subject")),
		AuthType:              "device_code",
		ExpiresIn:             tokenResp.ExpiresIn,
		AvailableModels:       models,
	}, nil
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

func unixMillisOrSecondsToTime(v int64) time.Time {
	if v <= 0 {
		return time.Time{}
	}
	if v > 1_000_000_000_000 {
		return time.UnixMilli(v).UTC()
	}
	return time.Unix(v, 0).UTC()
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
