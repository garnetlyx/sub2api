package service

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

const volcengineCodingAPIBaseSubstr = "volces.com/api/coding"

func isVolcengineCodingAccount(account *Account) bool {
	if account == nil || account.Type != AccountTypeAPIKey {
		return false
	}
	return strings.Contains(account.GetOpenAIBaseURL(), volcengineCodingAPIBaseSubstr)
}

// sanitizeBodyForVolcengineCoding downgrades forced
// {type:"function",function:{name:"..."}} tool_choice to "auto" so the
// Volcengine Ark Coding Plan gateway (which, like every reasoning-model
// carrier, rejects forced tool calls while reasoning is active) accepts the
// request. String forms ("auto"/"none"/"required") and the Anthropic-flavored
// {type:"allowed_tools",...} pass through unchanged. Returns (body, changed,
// err); when nothing matches the input is returned unchanged.
func sanitizeBodyForVolcengineCoding(body []byte) ([]byte, bool, error) {
	if len(body) == 0 {
		return body, false, nil
	}
	if !strings.Contains(string(body), `"tool_choice"`) {
		return body, false, nil
	}

	var reqBody map[string]any
	if err := json.Unmarshal(body, &reqBody); err != nil {
		return body, false, fmt.Errorf("volcengine coding sanitize: unmarshal: %w", err)
	}

	tc, ok := reqBody["tool_choice"]
	if !ok {
		return body, false, nil
	}
	normalized, did := normalizeVolcengineToolChoice(tc)
	if !did {
		return body, false, nil
	}
	reqBody["tool_choice"] = normalized

	out, err := json.Marshal(reqBody)
	if err != nil {
		return body, false, fmt.Errorf("volcengine coding sanitize: marshal: %w", err)
	}
	return out, true, nil
}

// normalizeVolcengineToolChoice downgrades the forced
// {type:"function",function:{name:"..."}} shape to "auto". String forms
// ("auto"/"none"/"required") and other object types pass through unchanged.
func normalizeVolcengineToolChoice(toolChoice any) (any, bool) {
	tcMap, ok := toolChoice.(map[string]any)
	if !ok {
		return toolChoice, false
	}
	if tcType, _ := tcMap["type"].(string); tcType != "function" {
		return toolChoice, false
	}
	return "auto", true
}

// hasForcedToolChoice reports whether body carries a forced tool_choice of the
// shape {type:"function",function:{name:"..."}} that every reasoning-model
// carrier in the current fleet rejects.
func hasForcedToolChoice(body []byte) bool {
	if !strings.Contains(string(body), `"tool_choice"`) {
		return false
	}
	var reqBody map[string]any
	if err := json.Unmarshal(body, &reqBody); err != nil {
		return false
	}
	tc, ok := reqBody["tool_choice"]
	if !ok {
		return false
	}
	_, did := normalizeVolcengineToolChoice(tc)
	return did
}

// downgradeForcedToolChoiceInBody rewrites the forced tool_choice object in
// body to the string "auto". Returns the new body and true when rewritten;
// the original body and false when no forced tool_choice was present.
func downgradeForcedToolChoiceInBody(body []byte) ([]byte, bool) {
	if len(body) == 0 || !strings.Contains(string(body), `"tool_choice"`) {
		return body, false
	}
	var reqBody map[string]any
	if err := json.Unmarshal(body, &reqBody); err != nil {
		return body, false
	}
	tc, ok := reqBody["tool_choice"]
	if !ok {
		return body, false
	}
	normalized, did := normalizeVolcengineToolChoice(tc)
	if !did {
		return body, false
	}
	reqBody["tool_choice"] = normalized
	out, err := json.Marshal(reqBody)
	if err != nil {
		return body, false
	}
	return out, true
}

// isOpenAIUpstreamForcedToolChoiceRejection reports whether an upstream 400
// matches a known forced-tool_choice rejection message shape. The classifier
// is intentionally broad on the message side; the caller must confirm the
// request body carries a forced tool_choice before retrying.
func isOpenAIUpstreamForcedToolChoiceRejection(statusCode int, upstreamMsg string) bool {
	if statusCode != http.StatusBadRequest {
		return false
	}
	msg := strings.ToLower(strings.TrimSpace(upstreamMsg))
	if msg == "" {
		return false
	}
	if strings.Contains(msg, "a parameter specified in the request is not valid") {
		return true
	}
	if strings.Contains(msg, "tool_choice") && strings.Contains(msg, "incompatible") {
		return true
	}
	return false
}