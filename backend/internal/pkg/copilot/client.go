package copilot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	deviceCodePath        = "/login/device/code"
	oauthTokenPath        = "/login/oauth/access_token"
	GitHubCopilotClientID = "Iv1.b507a08c87ecfe98"
	deviceGrantType       = "urn:ietf:params:oauth:grant-type:device_code"
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

type modelCapabilities struct {
	Type   string `json:"type"`
	Family string `json:"family"`
}

type Model struct {
	ID           string             `json:"id"`
	Capabilities modelCapabilities  `json:"capabilities"`
}

type modelsResponse struct {
	Data []Model `json:"data"`
}

type tokenResponse struct {
	Token     string          `json:"token"`
	ExpiresAt json.RawMessage `json:"expires_at"`
	ExpiresIn int64           `json:"expires_in"`
}

type DeviceCodeResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int64  `json:"expires_in"`
	Interval                int64  `json:"interval"`
}

type GitHubAccessTokenResponse struct {
	AccessToken           string `json:"access_token"`
	TokenType             string `json:"token_type"`
	Scope                 string `json:"scope"`
	RefreshToken          string `json:"refresh_token"`
	RefreshTokenExpiresIn int64  `json:"refresh_token_expires_in"`
	ExpiresIn             int64  `json:"expires_in"`
	Error                 string `json:"error"`
	Description           string `json:"error_description"`
	URI                   string `json:"error_uri"`
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

func StartDeviceCodeFlow(ctx context.Context, httpClient *http.Client) (*DeviceCodeResponse, error) {
	form := url.Values{}
	form.Set("client_id", GitHubCopilotClientID)
	form.Set("scope", "read:user")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://github.com"+deviceCodePath, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("github device code request failed: status %d body %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var payload DeviceCodeResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode github device code: %w", err)
	}
	if strings.TrimSpace(payload.DeviceCode) == "" || strings.TrimSpace(payload.UserCode) == "" {
		return nil, fmt.Errorf("github device code response missing required fields")
	}
	if payload.Interval <= 0 {
		payload.Interval = 5
	}
	return &payload, nil
}

func PollDeviceCodeAccessToken(ctx context.Context, httpClient *http.Client, deviceCode string) (*GitHubAccessTokenResponse, error) {
	form := url.Values{}
	form.Set("client_id", GitHubCopilotClientID)
	form.Set("device_code", strings.TrimSpace(deviceCode))
	form.Set("grant_type", deviceGrantType)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://github.com"+oauthTokenPath, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var payload GitHubAccessTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode github device token response: %w", err)
	}
	return &payload, nil
}

func GetGitHubUser(ctx context.Context, httpClient *http.Client, accessToken string) (*GitHubUser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, DefaultGitHubAPIBase+userPath, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(accessToken))
	req.Header.Set("Accept", "application/vnd.github+json")

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
	if len(payload.ExpiresAt) > 0 {
		// expires_at may be a Unix timestamp (number) or an RFC3339 string
		var ts int64
		if err := json.Unmarshal(payload.ExpiresAt, &ts); err == nil && ts > 0 {
			expiresAt = time.Unix(ts, 0)
		} else {
			var s string
			if err := json.Unmarshal(payload.ExpiresAt, &s); err == nil && s != "" {
				if parsed, err := time.Parse(time.RFC3339, s); err == nil {
					expiresAt = parsed
				}
			}
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

// RefreshGitHubToken exchanges a GitHub refresh_token for a new access_token + refresh_token pair.
func RefreshGitHubToken(ctx context.Context, httpClient *http.Client, refreshToken string) (*GitHubAccessTokenResponse, error) {
	form := url.Values{}
	form.Set("client_id", GitHubCopilotClientID)
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", strings.TrimSpace(refreshToken))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://github.com"+oauthTokenPath, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var payload GitHubAccessTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode github refresh token response: %w", err)
	}
	if payload.Error != "" {
		return nil, fmt.Errorf("%s: %s", payload.Error, payload.Description)
	}
	if strings.TrimSpace(payload.AccessToken) == "" {
		return nil, fmt.Errorf("github refresh token response returned empty access_token")
	}
	return &payload, nil
}

func ListModels(ctx context.Context, httpClient *http.Client, copilotToken string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, DefaultCopilotAPIBase+"/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(copilotToken))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Editor-Version", "vscode/1.99.3")
	req.Header.Set("Editor-Plugin-Version", "copilot-chat/0.26.7")
	req.Header.Set("Copilot-Integration-Id", "vscode-chat")

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
		// skip internal router entries (e.g. accounts/msft/routers/...)
		if strings.HasPrefix(id, "accounts/") {
			continue
		}
		// skip non-chat capability types when type is explicitly set
		capType := strings.TrimSpace(item.Capabilities.Type)
		if capType != "" && capType != "chat" {
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
