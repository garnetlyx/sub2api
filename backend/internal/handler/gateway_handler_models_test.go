package handler

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/stretchr/testify/require"
)

func TestGatewayModelListResponseIncludesCodexCatalog(t *testing.T) {
	models := []claude.Model{
		{
			ID:          "gpt-5.5",
			Type:        "model",
			DisplayName: "GPT-5.5",
		},
		{
			ID:          "  ",
			Type:        "model",
			DisplayName: "ignored",
		},
	}

	response := gatewayModelListResponse(models)

	require.Equal(t, "list", response["object"])
	require.Equal(t, models, response["data"])
	require.Equal(t, []codexGatewayModelInfo{
		{
			Slug:        "gpt-5.5",
			DisplayName: "GPT-5.5",
			SupportedReasoningLevels: []codexGatewayReasoningLevel{
				{Effort: "low", Description: "low"},
				{Effort: "medium", Description: "medium"},
				{Effort: "high", Description: "high"},
				{Effort: "xhigh", Description: "xhigh"},
			},
			ShellType:                  "default",
			Visibility:                 "list",
			SupportedInAPI:             true,
			Priority:                   1,
			BaseInstructions:           codexGatewayModelBaseInstructions,
			SupportsReasoningSummaries: false,
			SupportVerbosity:           false,
			TruncationPolicy: codexGatewayTruncationPolicy{
				Mode:  "bytes",
				Limit: 10000,
			},
			SupportsParallelToolCalls:  false,
			ContextWindow:              272000,
			ExperimentalSupportedTools: []string{},
			InputModalities:            []string{"text", "image"},
			SupportsSearchTool:         false,
		},
	}, response["models"])

	body, err := json.Marshal(response)
	require.NoError(t, err)

	var decoded struct {
		Object string         `json:"object"`
		Data   []claude.Model `json:"data"`
		Models []struct {
			Slug                     string `json:"slug"`
			DisplayName              string `json:"display_name"`
			SupportedReasoningLevels []struct {
				Effort      string `json:"effort"`
				Description string `json:"description"`
			} `json:"supported_reasoning_levels"`
			ShellType        string `json:"shell_type"`
			Visibility       string `json:"visibility"`
			SupportedInAPI   bool   `json:"supported_in_api"`
			Priority         int    `json:"priority"`
			BaseInstructions string `json:"base_instructions"`
			TruncationPolicy struct {
				Mode  string `json:"mode"`
				Limit int64  `json:"limit"`
			} `json:"truncation_policy"`
			SupportsParallelToolCalls  bool     `json:"supports_parallel_tool_calls"`
			ContextWindow              int64    `json:"context_window"`
			ExperimentalSupportedTools []string `json:"experimental_supported_tools"`
			InputModalities            []string `json:"input_modalities"`
		} `json:"models"`
	}
	require.NoError(t, json.Unmarshal(body, &decoded))
	require.Equal(t, "list", decoded.Object)
	require.Len(t, decoded.Data, 2)
	require.Len(t, decoded.Models, 1)
	require.Equal(t, "gpt-5.5", decoded.Models[0].Slug)
	require.Equal(t, "default", decoded.Models[0].ShellType)
	require.Equal(t, "list", decoded.Models[0].Visibility)
	require.True(t, decoded.Models[0].SupportedInAPI)
	require.Equal(t, "bytes", decoded.Models[0].TruncationPolicy.Mode)
	require.Equal(t, int64(10000), decoded.Models[0].TruncationPolicy.Limit)
	require.Equal(t, []string{"text", "image"}, decoded.Models[0].InputModalities)
}
