package kiro

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type OpenAIChatMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type OpenAIChatRequest struct {
	Model    string             `json:"model"`
	Messages []OpenAIChatMessage `json:"messages"`
	Stream   bool               `json:"stream"`
}

type OpenAIChatChoice struct {
	Index   int                    `json:"index"`
	Message *OpenAIChatChoiceMessage `json:"message,omitempty"`
	Delta   *OpenAIChatChoiceDelta   `json:"delta,omitempty"`
	FinishReason *string            `json:"finish_reason,omitempty"`
}

type OpenAIChatChoiceMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type OpenAIChatChoiceDelta struct {
	Role    string  `json:"role,omitempty"`
	Content *string `json:"content,omitempty"`
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
	ID      string             `json:"id"`
	Object  string             `json:"object"`
	Created int64              `json:"created"`
	Model   string             `json:"model"`
	Choices []OpenAIStreamChoice `json:"choices"`
}

type OpenAIStreamChoice struct {
	Index         int                    `json:"index"`
	Delta         *OpenAIChatChoiceDelta `json:"delta"`
	FinishReason  *string                `json:"finish_reason"`
}

func ConvertOpenAIToKiro(body []byte, profileArn string) (*GenerateAssistantResponseRequest, error) {
	result := gjson.ParseBytes(body)

	model := result.Get("model").String()
	kiroModel := ResolveModelID(model)

	var messages []OpenAIChatMessage
	result.Get("messages").ForEach(func(key, value gjson.Result) bool {
		msg := OpenAIChatMessage{
			Role:    value.Get("role").String(),
			Content: extractContent(value.Get("content")),
		}
		messages = append(messages, msg)
		return true
	})

	var history []HistoryItem
	var lastUserContent string
	var lastUserIdx int = -1

	for i, msg := range messages {
		if msg.Role == "user" {
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
					Content: contentStr,
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
						Content:   contentStr,
						MessageID: fmt.Sprintf("asst-%d", i),
					},
				})
			}

		case "user":
			if i < lastUserIdx {
				history = append(history, HistoryItem{
					Role: "user",
					UserInputMessage: &UserInputMessage{
						Content: contentStr,
					},
				})
			}
		}
	}

	_ = kiroModel

	req := &GenerateAssistantResponseRequest{
		ProfileArn: profileArn,
		ConversationState: &ConversationState{
			ChatTriggerType: "MANUAL",
			CurrentMessage: &CurrentMessage{
				UserInputMessage: &UserInputMessage{
					Content: lastUserContent,
				},
			},
			History: history,
		},
	}

	return req, nil
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
	return model
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
	now := time.Now().Unix()
	finishReason := "stop"
	resp := OpenAIChatResponse{
		ID:      fmt.Sprintf("chatcmpl-kiro-%d", now),
		Object:  "chat.completion",
		Created: now,
		Model:   model,
		Choices: []OpenAIChatChoice{
			{
				Index: 0,
				Message: &OpenAIChatChoiceMessage{
					Role:    "assistant",
					Content: content,
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
