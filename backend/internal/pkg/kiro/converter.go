package kiro

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type OpenAIChatMessage struct {
	Role       string           `json:"role"`
	Content    any              `json:"content"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	ToolCalls  []OpenAIToolCall `json:"tool_calls,omitempty"`
}

type OpenAIToolCall struct {
	ID       string             `json:"id"`
	Type     string             `json:"type,omitempty"`
	Function OpenAIToolFunction `json:"function"`
}

type OpenAIToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type OpenAIChatRequest struct {
	Model    string              `json:"model"`
	Messages []OpenAIChatMessage `json:"messages"`
	Stream   bool                `json:"stream"`
}

type OpenAIChatChoice struct {
	Index        int                      `json:"index"`
	Message      *OpenAIChatChoiceMessage `json:"message,omitempty"`
	Delta        *OpenAIChatChoiceDelta   `json:"delta,omitempty"`
	FinishReason *string                  `json:"finish_reason,omitempty"`
}

type OpenAIChatChoiceMessage struct {
	Role      string           `json:"role"`
	Content   string           `json:"content"`
	ToolCalls []OpenAIToolCall `json:"tool_calls,omitempty"`
}

type OpenAIChatChoiceDelta struct {
	Role      string                 `json:"role,omitempty"`
	Content   *string                `json:"content,omitempty"`
	ToolCalls []OpenAIStreamToolCall `json:"tool_calls,omitempty"`
}

type OpenAIStreamToolCall struct {
	Index    int                `json:"index"`
	ID       string             `json:"id,omitempty"`
	Type     string             `json:"type,omitempty"`
	Function OpenAIToolFunction `json:"function"`
}

type OpenAIChatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type OpenAIChatResponse struct {
	ID      string             `json:"id"`
	Object  string             `json:"object"`
	Created int64              `json:"created"`
	Model   string             `json:"model"`
	Choices []OpenAIChatChoice `json:"choices"`
	Usage   *OpenAIChatUsage   `json:"usage,omitempty"`
}

type OpenAIStreamChunk struct {
	ID      string               `json:"id"`
	Object  string               `json:"object"`
	Created int64                `json:"created"`
	Model   string               `json:"model"`
	Choices []OpenAIStreamChoice `json:"choices"`
}

type OpenAIStreamChoice struct {
	Index        int                    `json:"index"`
	Delta        *OpenAIChatChoiceDelta `json:"delta"`
	FinishReason *string                `json:"finish_reason"`
}

func ConvertOpenAIToKiro(body []byte, profileArn string) (*GenerateAssistantResponseRequest, error) {
	result := gjson.ParseBytes(body)

	model := result.Get("model").String()
	kiroModel := ResolveModelID(model)
	tools := convertOpenAITools(result.Get("tools"))

	var messages []OpenAIChatMessage
	result.Get("messages").ForEach(func(key, value gjson.Result) bool {
		msg := OpenAIChatMessage{
			Role:       value.Get("role").String(),
			Content:    extractContent(value.Get("content")),
			ToolCallID: value.Get("tool_call_id").String(),
		}
		if value.Get("tool_calls").IsArray() {
			_ = json.Unmarshal([]byte(value.Get("tool_calls").Raw), &msg.ToolCalls)
		}
		messages = append(messages, msg)
		return true
	})

	var history []HistoryItem
	var lastUserContent string
	var lastUserIdx int = -1

	for i, msg := range messages {
		if msg.Role == "user" || msg.Role == "tool" {
			lastUserContent = contentToString(msg.Content)
			lastUserIdx = i
		}
	}

	for i, msg := range messages {
		contentStr := contentToString(msg.Content)
		switch msg.Role {
		case "system":
			history = append(history, HistoryItem{
				Role: "user",
				UserInputMessage: &UserInputMessage{
					Content: contentOrFallback(contentStr),
					ModelID: kiroModel,
					Origin:  "AI_EDITOR",
				},
			})
			history = append(history, HistoryItem{
				Role: "assistant",
				AssistantResponseMessage: &AssistantResponseMessage{
					Content:   "Understood.",
					MessageID: fmt.Sprintf("sys-%d", i),
				},
			})

		case "assistant":
			if i < lastUserIdx {
				history = append(history, HistoryItem{
					Role: "assistant",
					AssistantResponseMessage: &AssistantResponseMessage{
						Content:   contentOrFallback(contentStr),
						MessageID: fmt.Sprintf("asst-%d", i),
						ToolUses:  convertOpenAIToolCalls(msg.ToolCalls),
					},
				})
			}

		case "user":
			if i < lastUserIdx {
				history = append(history, HistoryItem{
					Role: "user",
					UserInputMessage: &UserInputMessage{
						Content: contentOrFallback(contentStr),
						ModelID: kiroModel,
						Origin:  "AI_EDITOR",
					},
				})
			}
		case "tool":
			if i < lastUserIdx {
				history = append(history, HistoryItem{
					Role: "user",
					UserInputMessage: &UserInputMessage{
						Content:                 contentOrFallback(contentStr),
						ModelID:                 kiroModel,
						Origin:                  "AI_EDITOR",
						UserInputMessageContext: map[string]any{"toolResults": []KiroToolResult{openAIToolResultToKiro(msg)}},
					},
				})
			}
		}
	}

	currentContext := map[string]any{}
	if len(tools) > 0 {
		currentContext["tools"] = tools
	}
	if lastUserIdx >= 0 && messages[lastUserIdx].Role == "tool" {
		currentContext["toolResults"] = []KiroToolResult{openAIToolResultToKiro(messages[lastUserIdx])}
	}
	currentMessage := &UserInputMessage{
		Content: contentOrFallback(lastUserContent),
		ModelID: kiroModel,
		Origin:  "AI_EDITOR",
	}
	if len(currentContext) > 0 {
		currentMessage.UserInputMessageContext = currentContext
	}

	req := &GenerateAssistantResponseRequest{
		ProfileArn: profileArn,
		ConversationState: &ConversationState{
			ChatTriggerType: "MANUAL",
			CurrentMessage: &CurrentMessage{
				UserInputMessage: currentMessage,
			},
			History: history,
		},
	}

	return req, nil
}

func ConvertAnthropicToKiro(body []byte, profileArn string) (*GenerateAssistantResponseRequest, error) {
	var req apicompat.AnthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}

	kiroModel := ResolveModelID(req.Model)
	tools := convertAnthropicTools(req.Tools)

	var history []HistoryItem
	if systemText := anthropicSystemText(req.System); systemText != "" {
		history = append(history, HistoryItem{
			Role: "user",
			UserInputMessage: &UserInputMessage{
				Content: contentOrFallback(systemText),
				ModelID: kiroModel,
				Origin:  "AI_EDITOR",
			},
		})
		history = append(history, HistoryItem{
			Role: "assistant",
			AssistantResponseMessage: &AssistantResponseMessage{
				Content:   "Understood.",
				MessageID: "sys-0",
			},
		})
	}

	lastUserIdx := -1
	for i, msg := range req.Messages {
		if msg.Role == "user" {
			lastUserIdx = i
		}
	}

	lastUserContent := ""
	var currentToolResults []KiroToolResult
	for i, msg := range req.Messages {
		contentText := anthropicContentText(msg.Content)
		switch msg.Role {
		case "assistant":
			if i < lastUserIdx {
				history = append(history, HistoryItem{
					Role: "assistant",
					AssistantResponseMessage: &AssistantResponseMessage{
						Content:   contentOrFallback(contentText),
						MessageID: fmt.Sprintf("asst-%d", i),
						ToolUses:  anthropicToolUses(msg.Content),
					},
				})
			}
		case "user":
			toolResults := anthropicToolResults(msg.Content)
			if i == lastUserIdx {
				lastUserContent = contentText
				currentToolResults = toolResults
				continue
			}
			if i < lastUserIdx {
				userMessage := &UserInputMessage{
					Content: contentOrFallback(contentText),
					ModelID: kiroModel,
					Origin:  "AI_EDITOR",
				}
				if len(toolResults) > 0 {
					userMessage.UserInputMessageContext = map[string]any{"toolResults": toolResults}
				}
				history = append(history, HistoryItem{
					Role:             "user",
					UserInputMessage: userMessage,
				})
			}
		}
	}

	currentContext := map[string]any{}
	if len(tools) > 0 {
		currentContext["tools"] = tools
	}
	if len(currentToolResults) > 0 {
		currentContext["toolResults"] = currentToolResults
	}
	currentMessage := &UserInputMessage{
		Content: contentOrFallback(lastUserContent),
		ModelID: kiroModel,
		Origin:  "AI_EDITOR",
	}
	if len(currentContext) > 0 {
		currentMessage.UserInputMessageContext = currentContext
	}

	return &GenerateAssistantResponseRequest{
		ProfileArn: profileArn,
		ConversationState: &ConversationState{
			ChatTriggerType: "MANUAL",
			CurrentMessage: &CurrentMessage{
				UserInputMessage: currentMessage,
			},
			History: history,
		},
	}, nil
}

func ResolveModelID(model string) string {
	model = strings.TrimSpace(model)
	if mapped, ok := DefaultModelMapping[model]; ok {
		return mapped
	}
	for pattern, mapped := range DefaultModelMapping {
		if strings.HasPrefix(model, pattern) {
			return mapped
		}
	}
	return NormalizeKiroModelID(model)
}

func NormalizeKiroModelID(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	parts := strings.Split(model, "-")
	if len(parts) >= 4 && parts[0] == "claude" {
		family := parts[1]
		if (family == "haiku" || family == "sonnet" || family == "opus") && isDigits(parts[2]) && isOneOrTwoDigits(parts[3]) {
			suffix := ""
			if len(parts) > 4 {
				suffix = "-" + strings.Join(parts[4:], "-")
			}
			return strings.Join([]string{parts[0], parts[1], parts[2]}, "-") + "." + parts[3] + suffix
		}
	}
	return model
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}

func isOneOrTwoDigits(s string) bool {
	return len(s) >= 1 && len(s) <= 2 && isDigits(s)
}

func convertOpenAITools(tools gjson.Result) []KiroToolSpec {
	if !tools.IsArray() {
		return nil
	}
	out := make([]KiroToolSpec, 0)
	tools.ForEach(func(_, item gjson.Result) bool {
		if item.Get("type").String() != "function" {
			return true
		}
		fn := item.Get("function")
		name := strings.TrimSpace(fn.Get("name").String())
		if name == "" {
			return true
		}
		description := strings.TrimSpace(fn.Get("description").String())
		if description == "" {
			description = "Tool: " + name
		}
		var schema any = map[string]any{"type": "object", "properties": map[string]any{}}
		if raw := fn.Get("parameters").Raw; raw != "" {
			var parsed any
			if err := json.Unmarshal([]byte(raw), &parsed); err == nil && parsed != nil {
				schema = parsed
			}
		}
		out = append(out, KiroToolSpec{
			ToolSpecification: KiroToolSpecification{
				Name:        name,
				Description: description,
				InputSchema: KiroInputSchema{JSON: schema},
			},
		})
		return true
	})
	return out
}

func convertAnthropicTools(tools []apicompat.AnthropicTool) []KiroToolSpec {
	if len(tools) == 0 {
		return nil
	}
	out := make([]KiroToolSpec, 0, len(tools))
	for _, tool := range tools {
		name := strings.TrimSpace(tool.Name)
		if name == "" || strings.TrimSpace(tool.Type) != "" {
			continue
		}
		description := strings.TrimSpace(tool.Description)
		if description == "" {
			description = "Tool: " + name
		}
		var schema any = map[string]any{"type": "object", "properties": map[string]any{}}
		if len(tool.InputSchema) > 0 {
			var parsed any
			if err := json.Unmarshal(tool.InputSchema, &parsed); err == nil && parsed != nil {
				schema = parsed
			}
		}
		out = append(out, KiroToolSpec{
			ToolSpecification: KiroToolSpecification{
				Name:        name,
				Description: description,
				InputSchema: KiroInputSchema{JSON: schema},
			},
		})
	}
	return out
}

func convertOpenAIToolCalls(toolCalls []OpenAIToolCall) []KiroToolUse {
	if len(toolCalls) == 0 {
		return nil
	}
	out := make([]KiroToolUse, 0, len(toolCalls))
	for _, tc := range toolCalls {
		name := strings.TrimSpace(tc.Function.Name)
		if name == "" {
			continue
		}
		var input any = map[string]any{}
		if args := strings.TrimSpace(tc.Function.Arguments); args != "" {
			var parsed any
			if err := json.Unmarshal([]byte(args), &parsed); err == nil && parsed != nil {
				input = parsed
			}
		}
		out = append(out, KiroToolUse{Name: name, Input: input, ToolUseID: tc.ID})
	}
	return out
}

func openAIToolResultToKiro(msg OpenAIChatMessage) KiroToolResult {
	return KiroToolResult{
		Content:   []KiroToolResultContent{{Text: contentOrFallback(contentToString(msg.Content))}},
		Status:    "success",
		ToolUseID: msg.ToolCallID,
	}
}

func anthropicToolUses(content json.RawMessage) []KiroToolUse {
	blocks := anthropicContentBlocks(content)
	if len(blocks) == 0 {
		return nil
	}
	out := make([]KiroToolUse, 0, len(blocks))
	for _, block := range blocks {
		if block.Type != "tool_use" || strings.TrimSpace(block.Name) == "" {
			continue
		}
		var input any = map[string]any{}
		if len(block.Input) > 0 {
			var parsed any
			if err := json.Unmarshal(block.Input, &parsed); err == nil && parsed != nil {
				input = parsed
			}
		}
		out = append(out, KiroToolUse{Name: block.Name, Input: input, ToolUseID: block.ID})
	}
	return out
}

func anthropicToolResults(content json.RawMessage) []KiroToolResult {
	blocks := anthropicContentBlocks(content)
	if len(blocks) == 0 {
		return nil
	}
	out := make([]KiroToolResult, 0, len(blocks))
	for _, block := range blocks {
		if block.Type != "tool_result" || strings.TrimSpace(block.ToolUseID) == "" {
			continue
		}
		status := "success"
		if block.IsError {
			status = "error"
		}
		out = append(out, KiroToolResult{
			Content:   []KiroToolResultContent{{Text: contentOrFallback(anthropicToolResultText(block.Content))}},
			Status:    status,
			ToolUseID: block.ToolUseID,
		})
	}
	return out
}

func anthropicSystemText(system json.RawMessage) string {
	system = bytesTrimSpace(system)
	if len(system) == 0 || string(system) == "null" {
		return ""
	}
	var text string
	if err := json.Unmarshal(system, &text); err == nil {
		return strings.TrimSpace(text)
	}
	return anthropicContentText(system)
}

func anthropicContentText(content json.RawMessage) string {
	content = bytesTrimSpace(content)
	if len(content) == 0 || string(content) == "null" {
		return ""
	}
	var text string
	if err := json.Unmarshal(content, &text); err == nil {
		return strings.TrimSpace(text)
	}
	blocks := anthropicContentBlocks(content)
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		switch block.Type {
		case "text":
			if strings.TrimSpace(block.Text) != "" {
				parts = append(parts, block.Text)
			}
		case "tool_result":
			if text := anthropicToolResultText(block.Content); strings.TrimSpace(text) != "" {
				parts = append(parts, text)
			}
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func anthropicToolResultText(content json.RawMessage) string {
	content = bytesTrimSpace(content)
	if len(content) == 0 || string(content) == "null" {
		return ""
	}
	var text string
	if err := json.Unmarshal(content, &text); err == nil {
		return strings.TrimSpace(text)
	}
	blocks := anthropicContentBlocks(content)
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
			parts = append(parts, block.Text)
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func anthropicContentBlocks(content json.RawMessage) []apicompat.AnthropicContentBlock {
	var blocks []apicompat.AnthropicContentBlock
	if err := json.Unmarshal(content, &blocks); err == nil {
		return blocks
	}
	return nil
}

func bytesTrimSpace(b []byte) []byte {
	return []byte(strings.TrimSpace(string(b)))
}

func contentOrFallback(content string) string {
	content = strings.TrimSpace(content)
	if content == "" {
		return "(empty)"
	}
	return content
}

func extractContent(content gjson.Result) string {
	if content.Type == gjson.String {
		return content.String()
	}
	if content.IsArray() {
		var parts []string
		content.ForEach(func(_, v gjson.Result) bool {
			if v.Get("type").String() == "text" {
				parts = append(parts, v.Get("text").String())
			}
			return true
		})
		return strings.Join(parts, "\n")
	}
	return content.String()
}

func BuildOpenAIResponse(content string, model string, promptTokens int) ([]byte, error) {
	return BuildOpenAIResponseWithToolCalls(content, model, promptTokens, nil)
}

func BuildAnthropicResponse(content string, model string, toolCalls []OpenAIToolCall) ([]byte, error) {
	now := time.Now().Unix()
	blocks := anthropicBlocksFromKiro(content, toolCalls)
	stopReason := "end_turn"
	if len(toolCalls) > 0 {
		stopReason = "tool_use"
	}
	resp := apicompat.AnthropicResponse{
		ID:         fmt.Sprintf("msg_kiro_%d", now),
		Type:       "message",
		Role:       "assistant",
		Content:    blocks,
		Model:      model,
		StopReason: stopReason,
		Usage: apicompat.AnthropicUsage{
			InputTokens:  0,
			OutputTokens: len(content) / 4,
		},
	}
	return json.Marshal(resp)
}

func BuildAnthropicStreamEvent(evt apicompat.AnthropicStreamEvent) ([]byte, error) {
	sse, err := apicompat.ResponsesAnthropicEventToSSE(evt)
	if err != nil {
		return nil, err
	}
	return []byte(sse), nil
}

func AnthropicBlocksFromToolCalls(content string, toolCalls []OpenAIToolCall) []apicompat.AnthropicContentBlock {
	return anthropicBlocksFromKiro(content, toolCalls)
}

func anthropicBlocksFromKiro(content string, toolCalls []OpenAIToolCall) []apicompat.AnthropicContentBlock {
	blocks := make([]apicompat.AnthropicContentBlock, 0, 1+len(toolCalls))
	if strings.TrimSpace(content) != "" {
		blocks = append(blocks, apicompat.AnthropicContentBlock{
			Type: "text",
			Text: content,
		})
	}
	for _, call := range toolCalls {
		if strings.TrimSpace(call.Function.Name) == "" {
			continue
		}
		input := json.RawMessage(`{}`)
		if args := strings.TrimSpace(call.Function.Arguments); args != "" && json.Valid([]byte(args)) {
			input = json.RawMessage(args)
		}
		blocks = append(blocks, apicompat.AnthropicContentBlock{
			Type:  "tool_use",
			ID:    call.ID,
			Name:  call.Function.Name,
			Input: input,
		})
	}
	if len(blocks) == 0 {
		blocks = append(blocks, apicompat.AnthropicContentBlock{Type: "text", Text: ""})
	}
	return blocks
}

func BuildOpenAIResponseWithToolCalls(content string, model string, promptTokens int, toolCalls []OpenAIToolCall) ([]byte, error) {
	now := time.Now().Unix()
	finishReason := "stop"
	if len(toolCalls) > 0 {
		finishReason = "tool_calls"
	}
	resp := OpenAIChatResponse{
		ID:      fmt.Sprintf("chatcmpl-kiro-%d", now),
		Object:  "chat.completion",
		Created: now,
		Model:   model,
		Choices: []OpenAIChatChoice{
			{
				Index: 0,
				Message: &OpenAIChatChoiceMessage{
					Role:      "assistant",
					Content:   content,
					ToolCalls: toolCalls,
				},
				FinishReason: &finishReason,
			},
		},
		Usage: &OpenAIChatUsage{
			PromptTokens:     promptTokens,
			CompletionTokens: len(content) / 4,
			TotalTokens:      promptTokens + len(content)/4,
		},
	}
	return json.Marshal(resp)
}

func BuildOpenAIStreamChunk(content string, model string, isFirst bool, finishReason *string) ([]byte, error) {
	now := time.Now().Unix()
	chunk := OpenAIStreamChunk{
		ID:      fmt.Sprintf("chatcmpl-kiro-%d", now),
		Object:  "chat.completion.chunk",
		Created: now,
		Model:   model,
		Choices: []OpenAIStreamChoice{
			{
				Index: 0,
				Delta: &OpenAIChatChoiceDelta{
					Role:    "",
					Content: &content,
				},
				FinishReason: finishReason,
			},
		},
	}
	if isFirst {
		chunk.Choices[0].Delta.Role = "assistant"
	}
	return json.Marshal(chunk)
}

func BuildOpenAIStreamToolCallsChunk(toolCalls []OpenAIToolCall, model string) ([]byte, error) {
	now := time.Now().Unix()
	indexed := make([]OpenAIStreamToolCall, 0, len(toolCalls))
	for i, tc := range toolCalls {
		indexed = append(indexed, OpenAIStreamToolCall{
			Index: i,
			ID:    tc.ID,
			Type:  "function",
			Function: OpenAIToolFunction{
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			},
		})
	}
	chunk := OpenAIStreamChunk{
		ID:      fmt.Sprintf("chatcmpl-kiro-%d", now),
		Object:  "chat.completion.chunk",
		Created: now,
		Model:   model,
		Choices: []OpenAIStreamChoice{
			{
				Index:        0,
				Delta:        &OpenAIChatChoiceDelta{ToolCalls: indexed},
				FinishReason: nil,
			},
		},
	}
	return json.Marshal(chunk)
}

func InjectModelToBody(body []byte, model string) []byte {
	currentModel := gjson.GetBytes(body, "model").String()
	if currentModel == "" {
		result, _ := sjson.SetBytes(body, "model", model)
		return result
	}
	return body
}

func GetModelFromBody(body []byte) string {
	return gjson.GetBytes(body, "model").String()
}

func GetStreamFromBody(body []byte) bool {
	return gjson.GetBytes(body, "stream").Bool()
}

func contentToString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}
