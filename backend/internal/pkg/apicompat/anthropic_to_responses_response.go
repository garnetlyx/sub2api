package apicompat

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Non-streaming: AnthropicResponse → ResponsesResponse
// ---------------------------------------------------------------------------

// AnthropicToResponsesResponse converts an Anthropic Messages response into a
// Responses API response. This is the reverse of ResponsesToAnthropic and
// enables Anthropic upstream responses to be returned in OpenAI Responses format.
func AnthropicToResponsesResponse(resp *AnthropicResponse) *ResponsesResponse {
	id := resp.ID
	if id == "" {
		id = generateResponsesID()
	}

	out := &ResponsesResponse{
		ID:     id,
		Object: "response",
		Model:  resp.Model,
	}

	var outputs []ResponsesOutput
	var msgParts []ResponsesContentPart

	for _, block := range resp.Content {
		switch block.Type {
		case "thinking":
			if block.Thinking != "" {
				outputs = append(outputs, ResponsesOutput{
					Type: "reasoning",
					ID:   generateItemID(),
					Summary: []ResponsesSummary{{
						Type: "summary_text",
						Text: block.Thinking,
					}},
				})
			}
		case "text":
			if block.Text != "" {
				msgParts = append(msgParts, ResponsesContentPart{
					Type: "output_text",
					Text: block.Text,
				})
			}
		case "tool_use":
			args := "{}"
			if len(block.Input) > 0 {
				args = string(block.Input)
			}
			outputs = append(outputs, ResponsesOutput{
				Type:      "function_call",
				ID:        generateItemID(),
				CallID:    toResponsesCallID(block.ID),
				Name:      block.Name,
				Arguments: args,
				Status:    "completed",
			})
		}
	}

	// Assemble message output item from text parts
	if len(msgParts) > 0 {
		outputs = append(outputs, ResponsesOutput{
			Type:    "message",
			ID:      generateItemID(),
			Role:    "assistant",
			Content: msgParts,
			Status:  "completed",
		})
	}

	if len(outputs) == 0 {
		outputs = append(outputs, ResponsesOutput{
			Type:    "message",
			ID:      generateItemID(),
			Role:    "assistant",
			Content: []ResponsesContentPart{{Type: "output_text", Text: ""}},
			Status:  "completed",
		})
	}
	out.Output = outputs

	// Map stop_reason → status
	out.Status = anthropicStopReasonToResponsesStatus(resp.StopReason, resp.Content)
	if out.Status == "incomplete" {
		out.IncompleteDetails = &ResponsesIncompleteDetails{Reason: "max_output_tokens"}
	}

	// Usage
	out.Usage = &ResponsesUsage{
		InputTokens:  resp.Usage.InputTokens,
		OutputTokens: resp.Usage.OutputTokens,
		TotalTokens:  resp.Usage.InputTokens + resp.Usage.OutputTokens,
	}
	if resp.Usage.CacheReadInputTokens > 0 {
		out.Usage.InputTokensDetails = &ResponsesInputTokensDetails{
			CachedTokens: resp.Usage.CacheReadInputTokens,
		}
	}

	return out
}

// anthropicStopReasonToResponsesStatus maps Anthropic stop_reason to Responses status.
func anthropicStopReasonToResponsesStatus(stopReason string, blocks []AnthropicContentBlock) string {
	switch stopReason {
	case "max_tokens":
		return "incomplete"
	case "end_turn", "tool_use", "stop_sequence":
		return "completed"
	default:
		return "completed"
	}
}

// ---------------------------------------------------------------------------
// Streaming: AnthropicStreamEvent → []ResponsesStreamEvent (stateful converter)
// ---------------------------------------------------------------------------

// AnthropicEventToResponsesState tracks state for converting a sequence of
// Anthropic SSE events into Responses SSE events.
type AnthropicEventToResponsesState struct {
	ResponseID     string
	Model          string
	Created        int64
	SequenceNumber int

	// CreatedSent tracks whether response.created has been emitted.
	CreatedSent bool
	// CompletedSent tracks whether the terminal event has been emitted.
	CompletedSent bool

	// Current output tracking
	OutputIndex     int
	CurrentItemID   string
	CurrentItemType string // "message" | "function_call" | "reasoning"

	// For message output: accumulate text parts
	ContentIndex int
	CurrentText  string

	// For function_call: track per-output info
	CurrentCallID    string
	CurrentName      string
	CurrentArguments string

	// Completed output items for response.completed snapshot fidelity.
	Outputs []ResponsesOutput

	// Usage from message_delta
	InputTokens          int
	OutputTokens         int
	CacheReadInputTokens int

	// StopReason from message_delta (e.g. "max_tokens", "end_turn")
	StopReason string
}

// NewAnthropicEventToResponsesState returns an initialised stream state.
func NewAnthropicEventToResponsesState() *AnthropicEventToResponsesState {
	return &AnthropicEventToResponsesState{
		Created: time.Now().Unix(),
	}
}

// AnthropicEventToResponsesEvents converts a single Anthropic SSE event into
// zero or more Responses SSE events, updating state as it goes.
func AnthropicEventToResponsesEvents(
	evt *AnthropicStreamEvent,
	state *AnthropicEventToResponsesState,
) []ResponsesStreamEvent {
	switch evt.Type {
	case "message_start":
		return anthToResHandleMessageStart(evt, state)
	case "content_block_start":
		return anthToResHandleContentBlockStart(evt, state)
	case "content_block_delta":
		return anthToResHandleContentBlockDelta(evt, state)
	case "content_block_stop":
		return anthToResHandleContentBlockStop(evt, state)
	case "message_delta":
		return anthToResHandleMessageDelta(evt, state)
	case "message_stop":
		return anthToResHandleMessageStop(state)
	default:
		return nil
	}
}

// FinalizeAnthropicResponsesStream emits synthetic termination events if the
// stream ended without a proper message_stop.
func FinalizeAnthropicResponsesStream(state *AnthropicEventToResponsesState) []ResponsesStreamEvent {
	if !state.CreatedSent || state.CompletedSent {
		return nil
	}

	var events []ResponsesStreamEvent

	// Close any open item
	events = append(events, closeCurrentResponsesItem(state)...)

	// Emit response.completed
	events = append(events, makeResponsesCompletedEvent(state, "completed", nil))
	state.CompletedSent = true
	return events
}

// ResponsesEventToSSE formats a ResponsesStreamEvent as an SSE data line.
func ResponsesEventToSSE(evt ResponsesStreamEvent) (string, error) {
	payload, err := responsesEventPayload(evt)
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("event: %s\ndata: %s\n\n", evt.Type, data), nil
}

// --- internal handlers ---

func anthToResHandleMessageStart(evt *AnthropicStreamEvent, state *AnthropicEventToResponsesState) []ResponsesStreamEvent {
	if evt.Message != nil {
		state.ResponseID = evt.Message.ID
		if state.Model == "" {
			state.Model = evt.Message.Model
		}
		if evt.Message.Usage.InputTokens > 0 {
			state.InputTokens = evt.Message.Usage.InputTokens
		}
	}

	if state.CreatedSent {
		return nil
	}
	state.CreatedSent = true

	// Emit response.created
	return []ResponsesStreamEvent{makeResponsesCreatedEvent(state)}
}

func anthToResHandleContentBlockStart(evt *AnthropicStreamEvent, state *AnthropicEventToResponsesState) []ResponsesStreamEvent {
	if evt.ContentBlock == nil {
		return nil
	}

	var events []ResponsesStreamEvent

	switch evt.ContentBlock.Type {
	case "thinking":
		// Emit a real reasoning item lifecycle for native Anthropic thinking
		// blocks so strict OpenAI Responses clients can reconcile /think flows.
		if state.CurrentItemType != "" {
			events = append(events, closeCurrentResponsesItem(state)...)
		}
		state.CurrentItemID = generateItemID()
		state.CurrentItemType = "reasoning"
		state.ContentIndex = 0
		state.CurrentText = ""

		events = append(events, makeResponsesEvent(state, "response.output_item.added", &ResponsesStreamEvent{
			OutputIndex: state.OutputIndex,
			Item: &ResponsesOutput{
				Type:    "reasoning",
				ID:      state.CurrentItemID,
				Status:  "in_progress",
				Summary: []ResponsesSummary{},
			},
		}))
		return events

	case "text":
		// If we don't have an open message item, open one
		if state.CurrentItemType != "message" {
			if state.CurrentItemType != "" {
				events = append(events, closeCurrentResponsesItem(state)...)
			}
			state.CurrentItemID = generateItemID()
			state.CurrentItemType = "message"
			state.ContentIndex = 0
			state.CurrentText = ""

			events = append(events, makeResponsesEvent(state, "response.output_item.added", &ResponsesStreamEvent{
				OutputIndex: state.OutputIndex,
				Item: &ResponsesOutput{
					Type:    "message",
					ID:      state.CurrentItemID,
					Role:    "assistant",
					Status:  "in_progress",
					Content: []ResponsesContentPart{},
				},
			}))
			events = append(events, makeResponsesEvent(state, "response.content_part.added", &ResponsesStreamEvent{
				OutputIndex:  state.OutputIndex,
				ContentIndex: state.ContentIndex,
				ItemID:       state.CurrentItemID,
				Part: &ResponsesContentPart{
					Type: "output_text",
					Text: "",
				},
			}))
		}

	case "tool_use":
		// Close previous item if any
		events = append(events, closeCurrentResponsesItem(state)...)

		state.CurrentItemID = generateItemID()
		state.CurrentItemType = "function_call"
		state.CurrentCallID = toResponsesCallID(evt.ContentBlock.ID)
		state.CurrentName = evt.ContentBlock.Name
		state.CurrentArguments = ""

		events = append(events, makeResponsesEvent(state, "response.output_item.added", &ResponsesStreamEvent{
			OutputIndex: state.OutputIndex,
			Item: &ResponsesOutput{
				Type:   "function_call",
				ID:     state.CurrentItemID,
				CallID: state.CurrentCallID,
				Name:   state.CurrentName,
				Status: "in_progress",
			},
		}))
	}

	return events
}

func anthToResHandleContentBlockDelta(evt *AnthropicStreamEvent, state *AnthropicEventToResponsesState) []ResponsesStreamEvent {
	if evt.Delta == nil {
		return nil
	}

	switch evt.Delta.Type {
	case "text_delta":
		if evt.Delta.Text == "" {
			return nil
		}
		state.CurrentText += evt.Delta.Text
		return []ResponsesStreamEvent{makeResponsesEvent(state, "response.output_text.delta", &ResponsesStreamEvent{
			OutputIndex:  state.OutputIndex,
			ContentIndex: state.ContentIndex,
			Delta:        evt.Delta.Text,
			ItemID:       state.CurrentItemID,
		})}

	case "thinking_delta":
		if state.CurrentItemID == "" || evt.Delta.Thinking == "" {
			return nil
		}
		switch state.CurrentItemType {
		case "reasoning":
			state.CurrentText += evt.Delta.Thinking
			return []ResponsesStreamEvent{makeResponsesEvent(state, "response.reasoning_summary_text.delta", &ResponsesStreamEvent{
				OutputIndex:  state.OutputIndex,
				ItemID:       state.CurrentItemID,
				SummaryIndex: 0,
				Delta:        evt.Delta.Thinking,
			})}
		case "message":
			// Some Anthropic-compatible upstreams tunnel visible text through
			// thinking_delta inside an otherwise normal text block.
			state.CurrentText += evt.Delta.Thinking
			return []ResponsesStreamEvent{makeResponsesEvent(state, "response.output_text.delta", &ResponsesStreamEvent{
				OutputIndex:  state.OutputIndex,
				ContentIndex: state.ContentIndex,
				Delta:        evt.Delta.Thinking,
				ItemID:       state.CurrentItemID,
			})}
		default:
			return nil
		}

	case "input_json_delta":
		if evt.Delta.PartialJSON == "" {
			return nil
		}
		state.CurrentArguments += evt.Delta.PartialJSON
		return []ResponsesStreamEvent{makeResponsesEvent(state, "response.function_call_arguments.delta", &ResponsesStreamEvent{
			OutputIndex: state.OutputIndex,
			Delta:       evt.Delta.PartialJSON,
			ItemID:      state.CurrentItemID,
			CallID:      state.CurrentCallID,
			Name:        state.CurrentName,
		})}

	case "signature_delta":
		// Anthropic signature deltas have no Responses equivalent; skip
		return nil
	}

	return nil
}

func anthToResHandleContentBlockStop(evt *AnthropicStreamEvent, state *AnthropicEventToResponsesState) []ResponsesStreamEvent {
	switch state.CurrentItemType {
	case "reasoning":
		return []ResponsesStreamEvent{
			makeResponsesEvent(state, "response.reasoning_summary_text.done", &ResponsesStreamEvent{
				OutputIndex:  state.OutputIndex,
				ItemID:       state.CurrentItemID,
				SummaryIndex: 0,
				Text:         state.CurrentText,
			}),
		}

	case "function_call":
		// Emit function_call_arguments.done + output item done
		events := []ResponsesStreamEvent{
			makeResponsesEvent(state, "response.function_call_arguments.done", &ResponsesStreamEvent{
				OutputIndex: state.OutputIndex,
				ItemID:      state.CurrentItemID,
				CallID:      state.CurrentCallID,
				Name:        state.CurrentName,
			}),
		}
		events = append(events, closeCurrentResponsesItem(state)...)
		return events

	case "message":
		// Emit output_text.done and content_part.done so strict Responses clients
		// can reconcile the text part lifecycle before the message item closes.
		return []ResponsesStreamEvent{
			makeResponsesEvent(state, "response.output_text.done", &ResponsesStreamEvent{
				OutputIndex:  state.OutputIndex,
				ContentIndex: state.ContentIndex,
				ItemID:       state.CurrentItemID,
				Text:         state.CurrentText,
			}),
			makeResponsesEvent(state, "response.content_part.done", &ResponsesStreamEvent{
				OutputIndex:  state.OutputIndex,
				ContentIndex: state.ContentIndex,
				ItemID:       state.CurrentItemID,
				Part: &ResponsesContentPart{
					Type: "output_text",
					Text: state.CurrentText,
				},
			}),
		}
	}

	return nil
}

func anthToResHandleMessageDelta(evt *AnthropicStreamEvent, state *AnthropicEventToResponsesState) []ResponsesStreamEvent {
	// Update usage
	if evt.Usage != nil {
		state.OutputTokens = evt.Usage.OutputTokens
		if evt.Usage.CacheReadInputTokens > 0 {
			state.CacheReadInputTokens = evt.Usage.CacheReadInputTokens
		}
	}
	// Save stop_reason so message_stop can emit the correct terminal status.
	if evt.Delta != nil && evt.Delta.StopReason != "" {
		state.StopReason = evt.Delta.StopReason
	}

	return nil
}

func anthToResHandleMessageStop(state *AnthropicEventToResponsesState) []ResponsesStreamEvent {
	if state.CompletedSent {
		return nil
	}

	var events []ResponsesStreamEvent

	// Close any open item
	events = append(events, closeCurrentResponsesItem(state)...)

	// Derive status from stop_reason saved during message_delta.
	status := "completed"
	var incompleteDetails *ResponsesIncompleteDetails
	if state.StopReason == "max_tokens" {
		status = "incomplete"
		incompleteDetails = &ResponsesIncompleteDetails{Reason: "max_output_tokens"}
	}

	// Emit response.completed
	events = append(events, makeResponsesCompletedEvent(state, status, incompleteDetails))
	state.CompletedSent = true
	return events
}

// --- helper functions ---

func closeCurrentResponsesItem(state *AnthropicEventToResponsesState) []ResponsesStreamEvent {
	if state.CurrentItemType == "" {
		return nil
	}

	itemType := state.CurrentItemType
	itemID := state.CurrentItemID
	currentText := state.CurrentText
	currentCallID := state.CurrentCallID
	currentName := state.CurrentName
	currentArguments := state.CurrentArguments

	// Reset
	state.CurrentItemType = ""
	state.CurrentItemID = ""
	state.CurrentCallID = ""
	state.CurrentName = ""
	state.CurrentText = ""
	state.CurrentArguments = ""
	state.OutputIndex++
	state.ContentIndex = 0

	completedItem := ResponsesOutput{
		Type:   itemType,
		ID:     itemID,
		Status: "completed",
	}

	switch itemType {
	case "message":
		completedItem.Role = "assistant"
		completedItem.Content = []ResponsesContentPart{{
			Type: "output_text",
			Text: currentText,
		}}
	case "reasoning":
		completedItem.Summary = []ResponsesSummary{{
			Type: "summary_text",
			Text: currentText,
		}}
	case "function_call":
		completedItem.CallID = currentCallID
		completedItem.Name = currentName
		completedItem.Arguments = currentArguments
	}

	state.Outputs = append(state.Outputs, completedItem)

	return []ResponsesStreamEvent{makeResponsesEvent(state, "response.output_item.done", &ResponsesStreamEvent{
		OutputIndex: state.OutputIndex - 1, // Use the index before increment
		Item:        &completedItem,
	})}
}

func makeResponsesCreatedEvent(state *AnthropicEventToResponsesState) ResponsesStreamEvent {
	seq := state.SequenceNumber
	state.SequenceNumber++
	return ResponsesStreamEvent{
		Type:           "response.created",
		SequenceNumber: seq,
		Response: &ResponsesResponse{
			ID:     state.ResponseID,
			Object: "response",
			Model:  state.Model,
			Status: "in_progress",
			Output: []ResponsesOutput{},
		},
	}
}

func makeResponsesCompletedEvent(
	state *AnthropicEventToResponsesState,
	status string,
	incompleteDetails *ResponsesIncompleteDetails,
) ResponsesStreamEvent {
	seq := state.SequenceNumber
	state.SequenceNumber++

	usage := &ResponsesUsage{
		InputTokens:  state.InputTokens,
		OutputTokens: state.OutputTokens,
		TotalTokens:  state.InputTokens + state.OutputTokens,
	}
	if state.CacheReadInputTokens > 0 {
		usage.InputTokensDetails = &ResponsesInputTokensDetails{
			CachedTokens: state.CacheReadInputTokens,
		}
	}

	return ResponsesStreamEvent{
		Type:           "response.completed",
		SequenceNumber: seq,
		Response: &ResponsesResponse{
			ID:                state.ResponseID,
			Object:            "response",
			Model:             state.Model,
			Status:            status,
			Output:            append([]ResponsesOutput(nil), state.Outputs...),
			Usage:             usage,
			IncompleteDetails: incompleteDetails,
		},
	}
}

func makeResponsesEvent(state *AnthropicEventToResponsesState, eventType string, template *ResponsesStreamEvent) ResponsesStreamEvent {
	seq := state.SequenceNumber
	state.SequenceNumber++

	evt := *template
	evt.Type = eventType
	evt.SequenceNumber = seq
	return evt
}

func generateResponsesID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "resp_" + hex.EncodeToString(b)
}

func generateItemID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "item_" + hex.EncodeToString(b)
}

func responsesEventPayload(evt ResponsesStreamEvent) (map[string]any, error) {
	payload := map[string]any{
		"type": evt.Type,
	}
	if evt.SequenceNumber != 0 || strings.HasPrefix(evt.Type, "response.") {
		payload["sequence_number"] = evt.SequenceNumber
	}

	switch evt.Type {
	case "response.created", "response.completed", "response.failed", "response.incomplete":
		payload["response"] = evt.Response
	case "response.output_item.added", "response.output_item.done":
		payload["output_index"] = evt.OutputIndex
		if evt.Item != nil {
			item := map[string]any{
				"type": evt.Item.Type,
			}
			if evt.Item.ID != "" {
				item["id"] = evt.Item.ID
			}
			if evt.Item.Role != "" {
				item["role"] = evt.Item.Role
			}
			if evt.Item.Status != "" {
				item["status"] = evt.Item.Status
			}
			if evt.Item.Type == "message" {
				content := make([]any, 0, len(evt.Item.Content))
				for _, part := range evt.Item.Content {
					partPayload := map[string]any{
						"type": part.Type,
						"text": part.Text,
					}
					content = append(content, partPayload)
				}
				item["content"] = content
			}
			if evt.Item.Type == "reasoning" {
				summary := make([]any, 0, len(evt.Item.Summary))
				for _, part := range evt.Item.Summary {
					partPayload := map[string]any{
						"type": part.Type,
						"text": part.Text,
					}
					summary = append(summary, partPayload)
				}
				item["summary"] = summary
			}
			if evt.Item.CallID != "" {
				item["call_id"] = evt.Item.CallID
			}
			if evt.Item.Name != "" {
				item["name"] = evt.Item.Name
			}
			if evt.Item.Arguments != "" {
				item["arguments"] = evt.Item.Arguments
			}
			payload["item"] = item
		}
	case "response.content_part.added", "response.content_part.done":
		payload["output_index"] = evt.OutputIndex
		payload["content_index"] = evt.ContentIndex
		payload["item_id"] = evt.ItemID
		part := map[string]any{
			"type": evt.Part.Type,
			"text": evt.Part.Text,
		}
		payload["part"] = part
	case "response.output_text.delta":
		payload["output_index"] = evt.OutputIndex
		payload["content_index"] = evt.ContentIndex
		payload["item_id"] = evt.ItemID
		payload["delta"] = evt.Delta
	case "response.output_text.done":
		payload["output_index"] = evt.OutputIndex
		payload["content_index"] = evt.ContentIndex
		payload["item_id"] = evt.ItemID
		payload["text"] = evt.Text
	case "response.function_call_arguments.delta", "response.function_call_arguments.done":
		payload["output_index"] = evt.OutputIndex
		payload["item_id"] = evt.ItemID
		payload["call_id"] = evt.CallID
		payload["name"] = evt.Name
		if evt.Delta != "" {
			payload["delta"] = evt.Delta
		}
		if evt.Arguments != "" {
			payload["arguments"] = evt.Arguments
		}
	case "response.reasoning_summary_text.delta", "response.reasoning_summary_text.done":
		payload["output_index"] = evt.OutputIndex
		payload["item_id"] = evt.ItemID
		payload["summary_index"] = evt.SummaryIndex
		if evt.Delta != "" {
			payload["delta"] = evt.Delta
		}
		if evt.Text != "" {
			payload["text"] = evt.Text
		}
	default:
		raw, err := json.Marshal(evt)
		if err != nil {
			return nil, err
		}
		var generic map[string]any
		if err := json.Unmarshal(raw, &generic); err != nil {
			return nil, err
		}
		return generic, nil
	}

	return payload, nil
}
