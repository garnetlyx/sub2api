package kiro

import (
	"encoding/json"
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertOpenAIToKiroIncludesToolsAndModelID(t *testing.T) {
	body := []byte(`{
		"model":"claude-sonnet-4-5",
		"messages":[{"role":"user","content":"run pwd"}],
		"tools":[{"type":"function","function":{"name":"executeBash","description":"Run shell","parameters":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}}}]
	}`)

	req, err := ConvertOpenAIToKiro(body, "arn:test")
	if err != nil {
		t.Fatalf("ConvertOpenAIToKiro returned error: %v", err)
	}
	raw, _ := json.Marshal(req)

	if got := gjson.GetBytes(raw, "conversationState.currentMessage.userInputMessage.modelId").String(); got != "claude-sonnet-4.5" {
		t.Fatalf("expected dotted model id, got %q", got)
	}
	if got := gjson.GetBytes(raw, "conversationState.currentMessage.userInputMessage.origin").String(); got != "AI_EDITOR" {
		t.Fatalf("expected AI_EDITOR origin, got %q", got)
	}
	if got := gjson.GetBytes(raw, "conversationState.currentMessage.userInputMessage.userInputMessageContext.tools.0.toolSpecification.name").String(); got != "executeBash" {
		t.Fatalf("expected tool spec, got %q in %s", got, raw)
	}
}

func TestConvertOpenAIToKiroIncludesToolResultsAndPriorToolUses(t *testing.T) {
	body := []byte(`{
		"model":"claude-sonnet-4",
		"messages":[
			{"role":"user","content":"run pwd"},
			{"role":"assistant","content":"","tool_calls":[{"id":"tooluse_1","type":"function","function":{"name":"executeBash","arguments":"{\"command\":\"pwd\"}"}}]},
			{"role":"tool","tool_call_id":"tooluse_1","content":"/tmp/work"}
		],
		"tools":[{"type":"function","function":{"name":"executeBash","parameters":{"type":"object"}}}]
	}`)

	req, err := ConvertOpenAIToKiro(body, "")
	if err != nil {
		t.Fatalf("ConvertOpenAIToKiro returned error: %v", err)
	}
	raw, _ := json.Marshal(req)

	if got := gjson.GetBytes(raw, "conversationState.history.1.assistantResponseMessage.toolUses.0.name").String(); got != "executeBash" {
		t.Fatalf("expected prior assistant toolUse, got %q in %s", got, raw)
	}
	if got := gjson.GetBytes(raw, "conversationState.currentMessage.userInputMessage.userInputMessageContext.toolResults.0.toolUseId").String(); got != "tooluse_1" {
		t.Fatalf("expected current tool result, got %q in %s", got, raw)
	}
}
