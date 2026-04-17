package service

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/copilot"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

type CopilotImportResult struct {
	GitHubLogin     string   `json:"github_login"`
	GitHubUserID    int64    `json:"github_user_id,omitempty"`
	Email           string   `json:"email,omitempty"`
	Name            string   `json:"name,omitempty"`
	AccessToken     string   `json:"access_token"`
	AvailableModels []string `json:"available_models,omitempty"`
}

type CopilotOAuthService struct {
	proxyRepo ProxyRepository
}

func NewCopilotOAuthService(proxyRepo ProxyRepository) *CopilotOAuthService {
	return &CopilotOAuthService{proxyRepo: proxyRepo}
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

	models, err := copilot.ListModels(ctx, httpClient, copilotToken.Token)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusBadGateway, "COPILOT_MODELS_LOOKUP_FAILED", "copilot models lookup failed: %v", err)
	}

	return &CopilotImportResult{
		GitHubLogin:     user.Login,
		GitHubUserID:    user.ID,
		Email:           user.Email,
		Name:            coalesceTrimmed(user.Name, user.Login),
		AccessToken:     accessToken,
		AvailableModels: models,
	}, nil
}

func (s *CopilotOAuthService) BuildAccountCredentials(result *CopilotImportResult) map[string]any {
	if result == nil {
		return nil
	}
	return map[string]any{
		"access_token": result.AccessToken,
	}
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
	if len(result.AvailableModels) > 0 {
		extra["available_models"] = result.AvailableModels
	}
	return extra
}

func coalesceTrimmed(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
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
