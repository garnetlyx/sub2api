package kiro

import (
	"strings"
	"time"
)

const (
	DefaultRegion           = "us-east-1"
	DefaultRedirectURI = "kiro://kiro.kiroAgent/authenticate-success"
	KiroIDEVersion          = "1.6.0"
	KiroIDEUserAgentPrefix  = "KiroIDE"
	AccessTokenRefreshSkew  = 5 * time.Minute
)

func AuthEndpoint(region string) string {
	return "https://prod." + region + ".auth.desktop.kiro.dev"
}

func QAPIEndpoint(region string) string {
	return "https://q." + region + ".amazonaws.com"
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

type RefreshTokenRequest struct {
	RefreshToken string `json:"refreshToken"`
}

type OAuthTokenRequest struct {
	Code         string `json:"code"`
	CodeVerifier string `json:"code_verifier"`
	RedirectURI  string `json:"redirect_uri"`
}

type GenerateAssistantResponseRequest struct {
	ProfileArn       string            `json:"profileArn"`
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
	UserInputMessageContext any    `json:"userInputMessageContext,omitempty"`
}

type HistoryItem struct {
	Role                 string               `json:"role"`
	UserInputMessage     *UserInputMessage    `json:"userInputMessage,omitempty"`
	AssistantResponseMessage *AssistantResponseMessage `json:"assistantResponseMessage,omitempty"`
}

type AssistantResponseMessage struct {
	MessageID string `json:"messageId"`
	Content   string `json:"content"`
}

type ModelInfo struct {
	ModelID           string `json:"modelId"`
	ModelDisplayName  string `json:"modelDisplayName,omitempty"`
	ProviderName      string `json:"providerName,omitempty"`
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

var DefaultModelMapping = map[string]string{
	"claude-sonnet-4-5-20250929":  "CLAUDE_SONNET_4_5_20250929_V1_0",
	"claude-sonnet-4-5":           "CLAUDE_SONNET_4_5_20250929_V1_0",
	"claude-opus-4-20250514":      "CLAUDE_OPUS_4_20250514_V1_0",
	"claude-opus-4":               "CLAUDE_OPUS_4_20250514_V1_0",
	"claude-haiku-4-5-20251001":   "CLAUDE_HAIKU_4_5_20251001_V1_0",
	"claude-haiku-4-5":            "CLAUDE_HAIKU_4_5_20251001_V1_0",
	"claude-sonnet-4-6":           "CLAUDE_SONNET_4_6",
	"claude-opus-4-6":             "CLAUDE_OPUS_4_6",
}
