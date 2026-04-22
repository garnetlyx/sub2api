package kiro

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
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

type PKCEParams struct {
	CodeVerifier  string
	CodeChallenge string
	State         string
}

func GeneratePKCE() (*PKCEParams, error) {
	verifierBytes := make([]byte, 32)
	if _, err := rand.Read(verifierBytes); err != nil {
		return nil, fmt.Errorf("generate code verifier: %w", err)
	}
	codeVerifier := base64.RawURLEncoding.EncodeToString(verifierBytes)

	hash := sha256.Sum256([]byte(codeVerifier))
	codeChallenge := base64.RawURLEncoding.EncodeToString(hash[:])

	stateBytes := make([]byte, 16)
	if _, err := rand.Read(stateBytes); err != nil {
		return nil, fmt.Errorf("generate state: %w", err)
	}
	state := base64.RawURLEncoding.EncodeToString(stateBytes)

	return &PKCEParams{
		CodeVerifier:  codeVerifier,
		CodeChallenge: codeChallenge,
		State:         state,
	}, nil
}

func BuildAuthURL(region string, idp SocialProvider, redirectURI string, pkce *PKCEParams) string {
	base := AuthEndpoint(region)
	params := url.Values{}
	params.Set("idp", string(idp))
	params.Set("redirect_uri", redirectURI)
	params.Set("code_challenge", pkce.CodeChallenge)
	params.Set("code_challenge_method", "S256")
	params.Set("state", pkce.State)
	params.Set("prompt", "select_account")
	return base + "/login?" + params.Encode()
}

func ExchangeCode(ctx context.Context, httpClient *http.Client, region string, code string, codeVerifier string, redirectURI string) (*TokenExchangeResponse, error) {
	endpoint := AuthEndpoint(region) + "/oauth/token"

	reqBody := OAuthTokenRequest{
		Code:         code,
		CodeVerifier: codeVerifier,
		RedirectURI:  redirectURI,
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal token request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(bodyBytes)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", KiroIDEUserAgentPrefix+"/"+KiroIDEVersion)
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token exchange request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 65536))
	if err != nil {
		return nil, fmt.Errorf("read token response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token exchange failed: status %d body %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var tokenResp TokenExchangeResponse
	if err := json.Unmarshal(respBody, &tokenResp); err != nil {
		return nil, fmt.Errorf("decode token response: %w", err)
	}
	if strings.TrimSpace(tokenResp.AccessToken) == "" {
		return nil, fmt.Errorf("token exchange returned empty accessToken")
	}
	if strings.TrimSpace(tokenResp.RefreshToken) == "" {
		return nil, fmt.Errorf("token exchange returned empty refreshToken")
	}

	return &tokenResp, nil
}

func RefreshSocialToken(ctx context.Context, httpClient *http.Client, region string, refreshToken string) (*TokenExchangeResponse, error) {
	endpoint := AuthEndpoint(region) + "/refreshToken"

	reqBody := RefreshTokenRequest{RefreshToken: refreshToken}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal refresh request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(bodyBytes)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", KiroIDEUserAgentPrefix+"/"+KiroIDEVersion)
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("refresh token request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 65536))
	if err != nil {
		return nil, fmt.Errorf("read refresh response: %w", err)
	}

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("refresh token expired or invalid")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("refresh token failed: status %d body %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var tokenResp TokenExchangeResponse
	if err := json.Unmarshal(respBody, &tokenResp); err != nil {
		return nil, fmt.Errorf("decode refresh response: %w", err)
	}
	if strings.TrimSpace(tokenResp.AccessToken) == "" {
		return nil, fmt.Errorf("refresh response returned empty accessToken")
	}

	return &tokenResp, nil
}

func ListModels(ctx context.Context, httpClient *http.Client, region string, accessToken string) ([]ModelInfo, error) {
	endpoint := QAPIEndpoint(region) + "/listAvailableModels"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list models request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 65536))
	if err != nil {
		return nil, fmt.Errorf("read models response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list models failed: status %d body %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var models []ModelInfo
	if err := json.Unmarshal(respBody, &models); err != nil {
		return nil, fmt.Errorf("decode models response: %w", err)
	}
	return models, nil
}

func GenerateAssistantResponse(ctx context.Context, httpClient *http.Client, region string, accessToken string, reqBody *GenerateAssistantResponseRequest) (*http.Response, error) {
	endpoint := QAPIEndpoint(region) + "/generateAssistantResponse"

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(bodyBytes)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream, application/json")
	req.Header.Set("User-Agent", fmt.Sprintf("%s/%s-%s", KiroIDEUserAgentPrefix, KiroIDEVersion, "node"))

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("generate response request: %w", err)
	}
	return resp, nil
}
