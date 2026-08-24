package service

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/copilot"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
)

type CopilotImportResult struct {
	GitHubLogin           string    `json:"github_login"`
	GitHubUserID          int64     `json:"github_user_id,omitempty"`
	Email                 string    `json:"email,omitempty"`
	Name                  string    `json:"name,omitempty"`
	AccessToken           string    `json:"access_token"`
	RefreshToken          string    `json:"refresh_token,omitempty"`
	ExpiresAt             time.Time `json:"expires_at,omitempty"`
	RefreshTokenExpiresAt time.Time `json:"refresh_token_expires_at,omitempty"`
}

type CopilotOAuthService struct {
	proxyRepo   ProxyRepository
	accountRepo AccountRepository
	sessions    *copilotDeviceSessionStore
}

func NewCopilotOAuthService(proxyRepo ProxyRepository, accountRepo AccountRepository) *CopilotOAuthService {
	return &CopilotOAuthService{
		proxyRepo:   proxyRepo,
		accountRepo: accountRepo,
		sessions:    newCopilotDeviceSessionStore(),
	}
}

// FindByGitHubLogin returns the first copilot account whose extra.github_login matches.
// Returns (nil, nil) if not found.
func (s *CopilotOAuthService) FindByGitHubLogin(ctx context.Context, login string) (*Account, error) {
	if s.accountRepo == nil || strings.TrimSpace(login) == "" {
		return nil, nil
	}
	accounts, err := s.accountRepo.FindByExtraField(ctx, "github_login", login)
	if err != nil {
		return nil, err
	}
	for i := range accounts {
		if accounts[i].Platform == PlatformCopilot {
			return &accounts[i], nil
		}
	}
	return nil, nil
}

func (s *CopilotOAuthService) ImportAccessToken(ctx context.Context, accessToken string, proxyID *int64) (*CopilotImportResult, error) {
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return nil, infraerrors.New(http.StatusBadRequest, "COPILOT_ACCESS_TOKEN_REQUIRED", "access_token is required")
	}

	var proxyURL string
	if proxyID != nil && s.proxyRepo != nil {
		proxy, err := s.proxyRepo.GetByID(ctx, *proxyID)
		if err != nil {
			return nil, infraerrors.Newf(http.StatusBadRequest, "COPILOT_PROXY_NOT_FOUND", "proxy not found: %v", err)
		}
		if proxy != nil {
			proxyURL = proxy.URL()
		}
	}

	httpClient, err := copilot.NewHTTPClient(proxyURL)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusBadRequest, "COPILOT_PROXY_INVALID", "invalid proxy: %v", err)
	}

	user, err := copilot.GetGitHubUser(ctx, httpClient, accessToken)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusUnauthorized, "COPILOT_GITHUB_TOKEN_INVALID", "github access token validation failed: %v", err)
	}

	copilotToken, err := copilot.ExchangeCopilotToken(ctx, httpClient, accessToken)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusBadGateway, "COPILOT_TOKEN_EXCHANGE_FAILED", "copilot token exchange failed: %v", err)
	}

	if _, err := copilot.ListModels(ctx, httpClient, copilotToken.Token); err != nil {
		return nil, infraerrors.Newf(http.StatusBadGateway, "COPILOT_MODELS_LOOKUP_FAILED", "copilot models lookup failed: %v", err)
	}

	return &CopilotImportResult{
		GitHubLogin:  user.Login,
		GitHubUserID: user.ID,
		Email:        user.Email,
		Name:         coalesceTrimmed(user.Name, user.Login),
		AccessToken:  accessToken,
	}, nil
}

func (s *CopilotOAuthService) BuildAccountCredentials(result *CopilotImportResult) map[string]any {
	if result == nil {
		return nil
	}
	creds := map[string]any{"access_token": result.AccessToken}
	if result.RefreshToken != "" {
		creds["refresh_token"] = result.RefreshToken
		if !result.RefreshTokenExpiresAt.IsZero() {
			creds["refresh_token_expires_at"] = result.RefreshTokenExpiresAt.UTC().Format(time.RFC3339)
		}
	}
	if !result.ExpiresAt.IsZero() {
		creds["expires_at"] = result.ExpiresAt.UTC().Format(time.RFC3339)
	}
	return creds
}

func (s *CopilotOAuthService) BuildAccountExtra(result *CopilotImportResult) map[string]any {
	if result == nil {
		return nil
	}
	extra := map[string]any{
		"github_login": result.GitHubLogin,
	}
	if result.GitHubUserID > 0 {
		extra["github_user_id"] = result.GitHubUserID
	}
	if trimmed := strings.TrimSpace(result.Email); trimmed != "" {
		extra["email"] = trimmed
	}
	if trimmed := strings.TrimSpace(result.Name); trimmed != "" {
		extra["name"] = trimmed
	}
	return extra
}

type CopilotDeviceFlowStartResult struct {
	SessionID               string `json:"session_id"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete,omitempty"`
	ExpiresIn               int64  `json:"expires_in"`
	Interval                int64  `json:"interval"`
}

type copilotDeviceSession struct {
	DeviceCode string
	ProxyURL   string
	CreatedAt  time.Time
}

type copilotDeviceSessionStore struct {
	mu       sync.RWMutex
	sessions map[string]*copilotDeviceSession
	stopCh   chan struct{}
}

const copilotDeviceSessionTTL = 30 * time.Minute

func newCopilotDeviceSessionStore() *copilotDeviceSessionStore {
	store := &copilotDeviceSessionStore{
		sessions: make(map[string]*copilotDeviceSession),
		stopCh:   make(chan struct{}),
	}
	go store.cleanup()
	return store
}

func (s *copilotDeviceSessionStore) Set(sessionID string, session *copilotDeviceSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[sessionID] = session
}

func (s *copilotDeviceSessionStore) Get(sessionID string) (*copilotDeviceSession, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	session, ok := s.sessions[sessionID]
	if !ok {
		return nil, false
	}
	if time.Since(session.CreatedAt) > copilotDeviceSessionTTL {
		return nil, false
	}
	return session, true
}

func (s *copilotDeviceSessionStore) Delete(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, sessionID)
}

func (s *copilotDeviceSessionStore) cleanup() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			s.mu.Lock()
			for id, session := range s.sessions {
				if time.Since(session.CreatedAt) > copilotDeviceSessionTTL {
					delete(s.sessions, id)
				}
			}
			s.mu.Unlock()
		}
	}
}

func (s *CopilotOAuthService) StartDeviceFlow(ctx context.Context, proxyID *int64) (*CopilotDeviceFlowStartResult, error) {
	proxyURL, err := s.resolveProxyURL(ctx, proxyID)
	if err != nil {
		return nil, err
	}

	httpClient, err := copilot.NewHTTPClient(proxyURL)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusBadRequest, "COPILOT_PROXY_INVALID", "invalid proxy: %v", err)
	}

	deviceFlow, err := copilot.StartDeviceCodeFlow(ctx, httpClient)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusBadGateway, "COPILOT_DEVICE_CODE_START_FAILED", "failed to start device code flow: %v", err)
	}

	sessionID, err := randomSessionID()
	if err != nil {
		return nil, infraerrors.Newf(http.StatusInternalServerError, "COPILOT_DEVICE_SESSION_FAILED", "failed to create device session: %v", err)
	}
	s.sessions.Set(sessionID, &copilotDeviceSession{
		DeviceCode: deviceFlow.DeviceCode,
		ProxyURL:   proxyURL,
		CreatedAt:  time.Now(),
	})

	return &CopilotDeviceFlowStartResult{
		SessionID:               sessionID,
		UserCode:                deviceFlow.UserCode,
		VerificationURI:         deviceFlow.VerificationURI,
		VerificationURIComplete: deviceFlow.VerificationURIComplete,
		ExpiresIn:               deviceFlow.ExpiresIn,
		Interval:                deviceFlow.Interval,
	}, nil
}

func (s *CopilotOAuthService) PollDeviceFlow(ctx context.Context, sessionID string, proxyID *int64) (*CopilotImportResult, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, infraerrors.New(http.StatusBadRequest, "COPILOT_DEVICE_SESSION_REQUIRED", "session_id is required")
	}

	session, ok := s.sessions.Get(sessionID)
	if !ok {
		return nil, infraerrors.New(http.StatusBadRequest, "COPILOT_DEVICE_SESSION_NOT_FOUND", "device session not found or expired")
	}

	proxyURL := session.ProxyURL
	if proxyID != nil {
		resolved, err := s.resolveProxyURL(ctx, proxyID)
		if err != nil {
			return nil, err
		}
		proxyURL = resolved
	}

	httpClient, err := copilot.NewHTTPClient(proxyURL)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusBadRequest, "COPILOT_PROXY_INVALID", "invalid proxy: %v", err)
	}

	tokenResp, err := copilot.PollDeviceCodeAccessToken(ctx, httpClient, session.DeviceCode)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusBadGateway, "COPILOT_DEVICE_CODE_POLL_FAILED", "failed to poll device code flow: %v", err)
	}
	if tokenResp == nil {
		return nil, infraerrors.New(http.StatusBadGateway, "COPILOT_DEVICE_CODE_EMPTY", "device code flow returned empty response")
	}
	if tokenResp.Error != "" {
		switch tokenResp.Error {
		case "authorization_pending":
			return nil, infraerrors.New(http.StatusConflict, "COPILOT_DEVICE_AUTH_PENDING", "authorization is still pending")
		case "slow_down":
			return nil, infraerrors.New(http.StatusTooManyRequests, "COPILOT_DEVICE_AUTH_SLOW_DOWN", "authorization is pending, please wait a few seconds before retrying")
		case "expired_token":
			s.sessions.Delete(sessionID)
			return nil, infraerrors.New(http.StatusBadRequest, "COPILOT_DEVICE_AUTH_EXPIRED", "device code has expired, please restart authorization")
		case "access_denied":
			s.sessions.Delete(sessionID)
			return nil, infraerrors.New(http.StatusUnauthorized, "COPILOT_DEVICE_AUTH_DENIED", "device code authorization was denied")
		default:
			return nil, infraerrors.Newf(http.StatusBadGateway, "COPILOT_DEVICE_AUTH_FAILED", "device code authorization failed: %s", tokenResp.Description)
		}
	}
	if strings.TrimSpace(tokenResp.AccessToken) == "" {
		return nil, infraerrors.New(http.StatusBadGateway, "COPILOT_DEVICE_TOKEN_EMPTY", "github access token is empty")
	}

	result, err := s.ImportAccessToken(ctx, tokenResp.AccessToken, proxyID)
	if err != nil {
		return nil, err
	}
	s.sessions.Delete(sessionID)

	if tokenResp.RefreshToken != "" {
		result.RefreshToken = tokenResp.RefreshToken
		now := time.Now()
		if tokenResp.ExpiresIn > 0 {
			result.ExpiresAt = now.Add(time.Duration(tokenResp.ExpiresIn) * time.Second)
		}
		if tokenResp.RefreshTokenExpiresIn > 0 {
			result.RefreshTokenExpiresAt = now.Add(time.Duration(tokenResp.RefreshTokenExpiresIn) * time.Second)
		}
	}
	return result, nil
}

func (s *CopilotOAuthService) resolveProxyURL(ctx context.Context, proxyID *int64) (string, error) {
	if proxyID == nil || s.proxyRepo == nil {
		return "", nil
	}
	proxy, err := s.proxyRepo.GetByID(ctx, *proxyID)
	if err != nil {
		return "", infraerrors.Newf(http.StatusBadRequest, "COPILOT_PROXY_NOT_FOUND", "proxy not found: %v", err)
	}
	if proxy == nil {
		return "", nil
	}
	return proxy.URL(), nil
}

func randomSessionID() (string, error) {
	return openai.GenerateSessionID()
}

func coalesceTrimmed(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// RefreshByRefreshToken uses the stored GitHub refresh_token to get a new access_token,
// then re-validates the account against Copilot's live model endpoint.
func (s *CopilotOAuthService) RefreshByRefreshToken(ctx context.Context, account *Account, proxyID *int64) (*CopilotImportResult, error) {
	refreshToken := strings.TrimSpace(account.GetCredential("refresh_token"))
	if refreshToken == "" {
		return nil, infraerrors.New(http.StatusBadRequest, "COPILOT_NO_REFRESH_TOKEN", "no refresh_token stored for this account")
	}

	proxyURL, err := s.resolveProxyURL(ctx, proxyID)
	if err != nil {
		return nil, err
	}

	httpClient, err := copilot.NewHTTPClient(proxyURL)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusBadRequest, "COPILOT_PROXY_INVALID", "invalid proxy: %v", err)
	}

	tokenResp, err := copilot.RefreshGitHubToken(ctx, httpClient, refreshToken)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusUnauthorized, "COPILOT_REFRESH_FAILED", "github token refresh failed: %v", err)
	}

	result, err := s.ImportAccessToken(ctx, tokenResp.AccessToken, proxyID)
	if err != nil {
		return nil, err
	}

	result.RefreshToken = tokenResp.RefreshToken
	now := time.Now()
	if tokenResp.ExpiresIn > 0 {
		result.ExpiresAt = now.Add(time.Duration(tokenResp.ExpiresIn) * time.Second)
	}
	if tokenResp.RefreshTokenExpiresIn > 0 {
		result.RefreshTokenExpiresAt = now.Add(time.Duration(tokenResp.RefreshTokenExpiresIn) * time.Second)
	}
	return result, nil
}

func CopilotTokenCacheKey(account *Account) string {
	if account == nil {
		return "copilot:unknown"
	}
	login := strings.TrimSpace(account.GetExtraString("github_login"))
	if login != "" {
		return fmt.Sprintf("copilot:%s", strings.ToLower(login))
	}
	return fmt.Sprintf("copilot:%d", account.ID)
}
