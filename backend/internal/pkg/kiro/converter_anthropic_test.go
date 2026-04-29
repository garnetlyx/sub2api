package kiro

import (
	"encoding/json"
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertAnthropicToKiroIncludesToolsAndModelID(t *testing.T) {
	body := []byte(`{
		"model":"claude-sonnet-4-5",
		"max_tokens":64,
		"system":"Be concise.",
		"messages":[{"role":"user","content":[{"type":"text","text":"run pwd"}]}],
		"tools":[{"name":"executeBash","description":"Run shell","input_schema":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}}]
	}`)

	req, err := ConvertAnthropicToKiro(body, "arn:test")
	if err != nil {
		t.Fatalf("ConvertAnthropicToKiro returned error: %v", err)
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
	if got := gjson.GetBytes(raw, "conversationState.history.0.userInputMessage.content").String(); got != "Be concise." {
		t.Fatalf("expected system text in history, got %q in %s", got, raw)
	}
}

func TestConvertAnthropicToKiroIncludesToolResultsAndPriorToolUses(t *testing.T) {
	body := []byte(`{
		"model":"claude-sonnet-4.5",
		"max_tokens":64,
		"messages":[
			{"role":"user","content":"run pwd"},
			{"role":"assistant","content":[{"type":"text","text":"I'll run it."},{"type":"tool_use","id":"tooluse_1","name":"executeBash","input":{"command":"pwd"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"tooluse_1","content":"/tmp/work","is_error":false}]}
		],
		"tools":[{"name":"executeBash","input_schema":{"type":"object"}}]
	}`)

	req, err := ConvertAnthropicToKiro(body, "")
	if err != nil {
		t.Fatalf("ConvertAnthropicToKiro returned error: %v", err)
	}
	raw, _ := json.Marshal(req)

	if got := gjson.GetBytes(raw, "conversationState.history.1.assistantResponseMessage.toolUses.0.name").String(); got != "executeBash" {
		t.Fatalf("expected prior assistant toolUse, got %q in %s", got, raw)
	}
	if got := gjson.GetBytes(raw, "conversationState.currentMessage.userInputMessage.userInputMessageContext.toolResults.0.toolUseId").String(); got != "tooluse_1" {
		t.Fatalf("expected current tool result, got %q in %s", got, raw)
	}
	if got := gjson.GetBytes(raw, "conversationState.currentMessage.userInputMessage.userInputMessageContext.toolResults.0.content.0.text").String(); got != "/tmp/work" {
		t.Fatalf("expected tool result text, got %q in %s", got, raw)
	}
}

func TestBuildAnthropicResponseWithToolCallsUsesToolStopReason(t *testing.T) {
	resp, err := BuildAnthropicResponse("I'll run it.", "claude-sonnet-4.5", []OpenAIToolCall{{
		ID:   "tooluse_1",
		Type: "function",
		Function: OpenAIToolFunction{
			Name:      "executeBash",
			Arguments: `{"command":"pwd"}`,
		},
	}})
	if err != nil {
		t.Fatalf("BuildAnthropicResponse returned error: %v", err)
	}
	if got := gjson.GetBytes(resp, "stop_reason").String(); got != "tool_use" {
		t.Fatalf("expected tool_use stop reason, got %q in %s", got, resp)
	}
	if got := gjson.GetBytes(resp, "content.1.type").String(); got != "tool_use" {
		t.Fatalf("expected tool_use block, got %q in %s", got, resp)
	}
	if got := gjson.GetBytes(resp, "content.1.input.command").String(); got != "pwd" {
		t.Fatalf("expected pwd tool input, got %q in %s", got, resp)
	}
}
