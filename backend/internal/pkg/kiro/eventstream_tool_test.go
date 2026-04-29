package kiro

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestExtractToolCallsAggregatesKiroToolUseEvents(t *testing.T) {
	events := []EventStreamEvent{
		{Payload: []byte(`{"name":"executeBash","toolUseId":"tooluse_1"}`)},
		{Payload: []byte(`{"input":"{\"com","name":"executeBash","toolUseId":"tooluse_1"}`)},
		{Payload: []byte(`{"input":"mand\":\"pwd\"}","name":"executeBash","toolUseId":"tooluse_1"}`)},
		{Payload: []byte(`{"name":"executeBash","stop":true,"toolUseId":"tooluse_1"}`)},
	}

	calls := ExtractToolCalls(events)
	if len(calls) != 1 {
		t.Fatalf("expected one tool call, got %d", len(calls))
	}
	if calls[0].ID != "tooluse_1" {
		t.Fatalf("expected tooluse_1 id, got %q", calls[0].ID)
	}
	if calls[0].Function.Name != "executeBash" {
		t.Fatalf("expected executeBash, got %q", calls[0].Function.Name)
	}
	if calls[0].Function.Arguments != `{"command":"pwd"}` {
		t.Fatalf("expected normalized arguments, got %q", calls[0].Function.Arguments)
	}
}

func TestBuildOpenAIResponseWithToolCallsUsesToolFinishReason(t *testing.T) {
	resp, err := BuildOpenAIResponseWithToolCalls("", "claude-sonnet-4", 0, []OpenAIToolCall{{
		ID:   "tooluse_1",
		Type: "function",
		Function: OpenAIToolFunction{
			Name:      "executeBash",
			Arguments: `{"command":"pwd"}`,
		},
	}})
	if err != nil {
		t.Fatalf("BuildOpenAIResponseWithToolCalls returned error: %v", err)
	}
	if !gjsonGet(resp, "choices.0.message.tool_calls.0.function.name", "executeBash") {
		t.Fatalf("missing tool call in response: %s", resp)
	}
	if !gjsonGet(resp, "choices.0.finish_reason", "tool_calls") {
		t.Fatalf("missing tool finish reason in response: %s", resp)
	}
}

func gjsonGet(body []byte, path string, expected string) bool {
	return stringValue(body, path) == expected
}

func stringValue(body []byte, path string) string {
	return gjson.GetBytes(body, path).String()
}
