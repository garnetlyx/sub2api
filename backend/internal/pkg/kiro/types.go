package kiro

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
)

const (
	DefaultRegion          = "us-east-1"
	DefaultRedirectURI     = "kiro://kiro.kiroAgent/authenticate-success"
	KiroIDEVersion         = "1.6.0"
	KiroIDEUserAgentPrefix = "KiroIDE"
	AccessTokenRefreshSkew = 5 * time.Minute
)

func AuthEndpoint(region string) string {
	return "https://prod." + region + ".auth.desktop.kiro.dev"
}

func QAPIEndpoint(region string) string {
	return "https://q." + region + ".amazonaws.com"
}

func CodeWhispererEndpoint(region string) string {
	return "https://codewhisperer." + region + ".amazonaws.com"
}

type SocialProvider string

const (
	SocialGoogle SocialProvider = "Google"
	SocialGithub SocialProvider = "Github"
)

type TokenExchangeResponse struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ProfileArn   string `json:"profileArn"`
	ExpiresIn    int64  `json:"expiresIn"`
}

type DeviceCodeStartResponse struct {
	DeviceCode              string `json:"deviceCode"`
	UserCode                string `json:"userCode"`
	VerificationURI         string `json:"verificationUri"`
	VerificationURIComplete string `json:"verificationUriComplete"`
	ExpiresIn               int64  `json:"expiresIn"`
	Interval                int64  `json:"interval"`
}

type DeviceClientRegistrationResponse struct {
	ClientID              string `json:"clientId"`
	ClientSecret          string `json:"clientSecret"`
	ClientSecretExpiresAt int64  `json:"clientSecretExpiresAt"`
}

type OIDCTokenResponse struct {
	AccessToken           string `json:"accessToken"`
	RefreshToken          string `json:"refreshToken"`
	ExpiresIn             int64  `json:"expiresIn"`
	Error                 string `json:"error"`
	ErrorDescription      string `json:"error_description"`
	ClientSecretExpiresAt int64  `json:"clientSecretExpiresAt,omitempty"`
}

type RefreshTokenRequest struct {
	RefreshToken string `json:"refreshToken"`
}

type OAuthTokenRequest struct {
	Code         string `json:"code"`
	CodeVerifier string `json:"code_verifier"`
	RedirectURI  string `json:"redirect_uri"`
}

type GenerateAssistantResponseRequest struct {
	ProfileArn        string             `json:"profileArn,omitempty"`
	ConversationState *ConversationState `json:"conversationState"`
}

type ConversationState struct {
	ChatTriggerType string          `json:"chatTriggerType"`
	CurrentMessage  *CurrentMessage `json:"currentMessage,omitempty"`
	History         []HistoryItem   `json:"history,omitempty"`
}

type CurrentMessage struct {
	UserInputMessage *UserInputMessage `json:"userInputMessage,omitempty"`
}

type UserInputMessage struct {
	Content                 string `json:"content"`
	ModelID                 string `json:"modelId,omitempty"`
	Origin                  string `json:"origin,omitempty"`
	Images                  []any  `json:"images,omitempty"`
	UserInputMessageContext any    `json:"userInputMessageContext,omitempty"`
}

type HistoryItem struct {
	Role                     string                    `json:"role"`
	UserInputMessage         *UserInputMessage         `json:"userInputMessage,omitempty"`
	AssistantResponseMessage *AssistantResponseMessage `json:"assistantResponseMessage,omitempty"`
}

type AssistantResponseMessage struct {
	MessageID string `json:"messageId"`
	Content   string `json:"content"`
}

type ModelInfo struct {
	ModelID          string `json:"modelId"`
	ModelDisplayName string `json:"modelDisplayName,omitempty"`
	ProviderName     string `json:"providerName,omitempty"`
}

func ExtractRegionFromProfileArn(arn string) string {
	arn = strings.TrimSpace(arn)
	if arn == "" {
		return ""
	}
	// arn:aws:codewhisperer:{region}:account:profile/id
	parts := strings.SplitN(arn, ":", 6)
	if len(parts) >= 5 && parts[0] == "arn" && parts[1] == "aws" {
		region := strings.TrimSpace(parts[3])
		if region != "" && region != "account" {
			return region
		}
	}
	return ""
}

func ExtractSubjectFromAccessToken(token string) string {
	token = strings.TrimSpace(token)
	if token == "" {
		return ""
	}
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Subject string `json:"sub"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	return strings.TrimSpace(claims.Subject)
}

var DefaultModelMapping = map[string]string{
	"claude-sonnet-4-5-20250929": "claude-sonnet-4-5-20250929",
	"claude-sonnet-4-5":          "claude-sonnet-4-5-20250929",
	"claude-opus-4-20250514":     "claude-opus-4-20250514",
	"claude-opus-4":              "claude-opus-4-20250514",
	"claude-haiku-4-5-20251001":  "claude-haiku-4-5-20251001",
	"claude-haiku-4-5":           "claude-haiku-4-5-20251001",
	"claude-sonnet-4-6":          "claude-sonnet-4-6",
	"claude-opus-4-6":            "claude-opus-4-6",
}
