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
	"runtime"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/proxyutil"
	"github.com/google/uuid"
)

const (
	kiroCLIOrigin     = "KIRO_CLI"
	kiroCLIAppVersion = "1.28.1"
	kiroRestAmzTarget = "AmazonCodeWhispererService"
)

var qAPIEndpoint = QAPIEndpoint

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

func oidcEndpoint(region string) string {
	return "https://oidc." + strings.TrimSpace(region) + ".amazonaws.com"
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

func RegisterOIDCClient(ctx context.Context, httpClient *http.Client, region string, clientName string) (*DeviceClientRegistrationResponse, error) {
	endpoint := oidcEndpoint(region) + "/client/register"
	reqBody := map[string]any{
		"clientName": clientName,
		"clientType": "public",
		"scopes": []string{
			"codewhisperer:completions",
			"codewhisperer:analysis",
			"codewhisperer:conversations",
			"codewhisperer:transformations",
			"codewhisperer:taskassist",
		},
		"grantTypes": []string{"urn:ietf:params:oauth:grant-type:device_code", "refresh_token"},
		"issuerUrl":  "https://view.awsapps.com/start",
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal oidc registration request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(bodyBytes)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("oidc client register request: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 65536))
	if err != nil {
		return nil, fmt.Errorf("read oidc register response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("oidc client register failed: status %d body %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	var out DeviceClientRegistrationResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("decode oidc register response: %w", err)
	}
	if strings.TrimSpace(out.ClientID) == "" || strings.TrimSpace(out.ClientSecret) == "" {
		return nil, fmt.Errorf("oidc client register returned empty client credentials")
	}
	return &out, nil
}

func StartDeviceAuthorization(ctx context.Context, httpClient *http.Client, region, clientID, clientSecret string) (*DeviceCodeStartResponse, error) {
	endpoint := oidcEndpoint(region) + "/device_authorization"
	reqBody := map[string]any{
		"clientId":     strings.TrimSpace(clientID),
		"clientSecret": strings.TrimSpace(clientSecret),
		"startUrl":     "https://view.awsapps.com/start",
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal device authorization request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(bodyBytes)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("device authorization request: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 65536))
	if err != nil {
		return nil, fmt.Errorf("read device authorization response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("device authorization failed: status %d body %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	var out DeviceCodeStartResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("decode device authorization response: %w", err)
	}
	if strings.TrimSpace(out.DeviceCode) == "" || strings.TrimSpace(out.UserCode) == "" {
		return nil, fmt.Errorf("device authorization returned empty codes")
	}
	return &out, nil
}

func PollDeviceAuthorization(ctx context.Context, httpClient *http.Client, region, clientID, clientSecret, deviceCode string) (*OIDCTokenResponse, error) {
	endpoint := oidcEndpoint(region) + "/token"
	reqBody := map[string]any{
		"clientId":     strings.TrimSpace(clientID),
		"clientSecret": strings.TrimSpace(clientSecret),
		"grantType":    "urn:ietf:params:oauth:grant-type:device_code",
		"deviceCode":   strings.TrimSpace(deviceCode),
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal device token request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(bodyBytes)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("device token request: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 65536))
	if err != nil {
		return nil, fmt.Errorf("read device token response: %w", err)
	}
	var out OIDCTokenResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("decode device token response: %w", err)
	}
	if resp.StatusCode == http.StatusOK {
		return &out, nil
	}
	if strings.TrimSpace(out.Error) != "" {
		return &out, nil
	}
	return nil, fmt.Errorf("device token failed: status %d body %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
}

func RefreshOIDCToken(ctx context.Context, httpClient *http.Client, region, clientID, clientSecret, refreshToken string) (*OIDCTokenResponse, error) {
	endpoint := oidcEndpoint(region) + "/token"
	reqBody := map[string]any{
		"clientId":     strings.TrimSpace(clientID),
		"clientSecret": strings.TrimSpace(clientSecret),
		"grantType":    "refresh_token",
		"refreshToken": strings.TrimSpace(refreshToken),
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal oidc refresh request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(bodyBytes)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("oidc refresh request: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 65536))
	if err != nil {
		return nil, fmt.Errorf("read oidc refresh response: %w", err)
	}
	var out OIDCTokenResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("decode oidc refresh response: %w", err)
	}
	if resp.StatusCode == http.StatusOK {
		if strings.TrimSpace(out.AccessToken) == "" {
			return nil, fmt.Errorf("oidc refresh returned empty accessToken")
		}
		return &out, nil
	}
	if strings.TrimSpace(out.Error) != "" {
		return nil, fmt.Errorf("oidc refresh failed: %s", out.Error)
	}
	return nil, fmt.Errorf("oidc refresh failed: status %d body %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
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

func ListModels(ctx context.Context, httpClient *http.Client, region string, accessToken string, profileArn string) ([]ModelInfo, error) {
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return nil, fmt.Errorf("list models requires access token")
	}
	profileArn = strings.TrimSpace(profileArn)
	if profileArn == "" {
		return nil, fmt.Errorf("list models requires profile_arn")
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	params := url.Values{}
	params.Set("origin", kiroCLIOrigin)
	params.Set("profileArn", profileArn)
	endpoint := strings.TrimRight(qAPIEndpoint(region), "/") + "/?" + params.Encode()

	reqBody := map[string]string{
		"origin":     kiroCLIOrigin,
		"profileArn": profileArn,
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal models request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(bodyBytes)))
	if err != nil {
		return nil, err
	}
	setKiroAWSHeaders(req, accessToken, kiroRestAmzTarget+".ListAvailableModels", false)

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

	var payload struct {
		Models []ModelInfo `json:"models"`
	}
	if err := json.Unmarshal(respBody, &payload); err != nil {
		return nil, fmt.Errorf("decode models response: %w", err)
	}
	return payload.Models, nil
}

func setKiroAWSHeaders(req *http.Request, token string, amzTarget string, streaming bool) {
	apiName := "codewhispererruntime"
	if streaming {
		apiName = "codewhispererstreaming"
	}
	userAgent := fmt.Sprintf(
		"aws-sdk-rust/1.3.14 ua/2.1 api/%s/0.1.14474 os/%s lang/rust/1.92.0 md/appVersion-%s app/AmazonQ-For-CLI",
		apiName,
		runtime.GOOS,
		kiroCLIAppVersion,
	)
	xAmzUserAgent := fmt.Sprintf(
		"aws-sdk-rust/1.3.14 ua/2.1 api/%s/0.1.14474 os/%s lang/rust/1.92.0 m/F,C app/AmazonQ-For-CLI",
		apiName,
		runtime.GOOS,
	)
	req.Header.Set("Content-Type", "application/x-amz-json-1.0")
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	req.Header.Set("X-Amz-Target", amzTarget)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("X-Amz-User-Agent", xAmzUserAgent)
	req.Header.Set("X-Amzn-Codewhisperer-Optout", "false")
	req.Header.Set("Amz-Sdk-Invocation-Id", uuid.NewString())
	req.Header.Set("Amz-Sdk-Request", "attempt=1; max=3")
	req.Header.Set("Accept", "*/*")
}

func GenerateAssistantResponse(ctx context.Context, httpClient *http.Client, region string, accessToken string, reqBody *GenerateAssistantResponseRequest) (*http.Response, error) {
	endpoint := CodeWhispererEndpoint(region) + "/generateAssistantResponse"

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
