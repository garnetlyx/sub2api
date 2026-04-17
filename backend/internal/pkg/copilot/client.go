package copilot

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/proxyutil"
)

const (
	DefaultGitHubAPIBase  = "https://api.github.com"
	DefaultCopilotAPIBase = "https://api.githubcopilot.com"
	tokenExchangePath     = "/copilot_internal/v2/token"
	userPath              = "/user"
)

type GitHubUser struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
	Email string `json:"email"`
	Name  string `json:"name"`
}

type CopilotTokenInfo struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

type Model struct {
	ID string `json:"id"`
}

type modelsResponse struct {
	Data []Model `json:"data"`
}

type tokenResponse struct {
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at"`
	ExpiresIn int64  `json:"expires_in"`
}

func NewHTTPClient(proxyURL string) (*http.Client, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	trimmed := strings.TrimSpace(proxyURL)
	if trimmed == "" {
		return client, nil
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("parse proxy url: %w", err)
	}
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout: 10 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	if err := proxyutil.ConfigureTransportProxy(transport, parsed); err != nil {
		return nil, fmt.Errorf("configure proxy: %w", err)
	}
	client.Transport = transport
	return client, nil
}

func GetGitHubUser(ctx context.Context, httpClient *http.Client, accessToken string) (*GitHubUser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, DefaultGitHubAPIBase+userPath, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(accessToken))
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "sub2api-copilot/1.0")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github user lookup failed: status %d", resp.StatusCode)
	}

	var user GitHubUser
	if err := json.NewDecoder(resp.Body).Decode(&user); err != nil {
		return nil, fmt.Errorf("decode github user: %w", err)
	}
	if strings.TrimSpace(user.Login) == "" {
		return nil, fmt.Errorf("github user lookup returned empty login")
	}
	return &user, nil
}

func ExchangeCopilotToken(ctx context.Context, httpClient *http.Client, githubAccessToken string) (*CopilotTokenInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, DefaultGitHubAPIBase+tokenExchangePath, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(githubAccessToken))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "sub2api-copilot/1.0")
	req.Header.Set("Editor-Version", "vscode/1.99.3")
	req.Header.Set("Editor-Plugin-Version", "copilot-chat/0.26.7")
	req.Header.Set("Copilot-Integration-Id", "vscode-chat")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("copilot token exchange failed: status %d", resp.StatusCode)
	}

	var payload tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode copilot token: %w", err)
	}
	if strings.TrimSpace(payload.Token) == "" {
		return nil, fmt.Errorf("copilot token exchange returned empty token")
	}

	expiresAt := time.Time{}
	if strings.TrimSpace(payload.ExpiresAt) != "" {
		parsed, err := time.Parse(time.RFC3339, payload.ExpiresAt)
		if err == nil {
			expiresAt = parsed
		}
	}
	if expiresAt.IsZero() && payload.ExpiresIn > 0 {
		expiresAt = time.Now().Add(time.Duration(payload.ExpiresIn) * time.Second)
	}
	if expiresAt.IsZero() {
		expiresAt = time.Now().Add(25 * time.Minute)
	}

	return &CopilotTokenInfo{
		Token:     payload.Token,
		ExpiresAt: expiresAt,
	}, nil
}

func ListModels(ctx context.Context, httpClient *http.Client, copilotToken string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, DefaultCopilotAPIBase+"/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(copilotToken))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "sub2api-copilot/1.0")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("copilot models lookup failed: status %d", resp.StatusCode)
	}

	var payload modelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode copilot models: %w", err)
	}

	models := make([]string, 0, len(payload.Data))
	seen := make(map[string]struct{}, len(payload.Data))
	for _, item := range payload.Data {
		id := strings.TrimSpace(item.ID)
		if id == "" {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		models = append(models, id)
	}
	return models, nil
}
