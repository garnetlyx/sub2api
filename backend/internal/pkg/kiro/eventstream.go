package kiro

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

const (
	eventStreamHeaderTypeID  = 0x0
	eventStreamDataTypeID    = 0x05
	eventStreamMessageTypeID = 0x04
)

type EventStreamEvent struct {
	Headers map[string]string
	Payload []byte
}

type AssistantResponseEvent struct {
	AssistEventID          string `json:"assistEventId"`
	Content                string `json:"content"`
	ModelID                string `json:"modelId"`
	AssistantResponseEvent struct {
		Content string `json:"content"`
	} `json:"assistantResponseEvent"`
}

type ToolUseEvent struct {
	ToolUseID string `json:"toolUseId"`
	Name      string `json:"name"`
	Input     string `json:"input"`
	Stop      bool   `json:"stop"`

	ToolUseEvent struct {
		ToolUseID string `json:"toolUseId"`
		Name      string `json:"name"`
		Input     string `json:"input"`
		Stop      bool   `json:"stop"`
	} `json:"toolUseEvent"`
}

type ParsedEvent struct {
	Content string
	ToolUse *ToolUseEvent
}

func ParseEventStream(reader io.Reader) ([]EventStreamEvent, error) {
	var events []EventStreamEvent
	buf := bufio.NewReader(reader)

	for {
		prelude := make([]byte, 12)
		if _, err := io.ReadFull(buf, prelude); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			return events, fmt.Errorf("read prelude: %w", err)
		}

		totalLen := int(binary.BigEndian.Uint32(prelude[0:4]))
		headersLen := int(binary.BigEndian.Uint32(prelude[4:8]))
		if totalLen < 16 {
			return events, fmt.Errorf("invalid message length: %d", totalLen)
		}
		if headersLen < 0 || headersLen > totalLen-16 {
			return events, fmt.Errorf("invalid headers length: %d", headersLen)
		}

		remaining := make([]byte, totalLen-12)
		if _, err := io.ReadFull(buf, remaining); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			return events, fmt.Errorf("read message body: %w", err)
		}

		msgData := append(prelude, remaining...)

		preludeCRC := binary.BigEndian.Uint32(msgData[8:12])
		if err := validateCRC(msgData[:8], preludeCRC); err != nil {
			continue
		}
		headersStart := 12
		headersEnd := headersStart + headersLen
		payloadEnd := totalLen - 4
		headers := parseHeaders(msgData[headersStart:headersEnd])
		payload := msgData[headersEnd:payloadEnd]

		events = append(events, EventStreamEvent{
			Headers: headers,
			Payload: payload,
		})
	}

	return events, nil
}

func validateCRC(data []byte, expected uint32) error {
	return nil
}

func parseHeaders(data []byte) map[string]string {
	headers := make(map[string]string)
	offset := 0

	for offset < len(data) {
		if offset+2 > len(data) {
			break
		}
		headerNameLen := int(data[offset])
		offset++
		if offset+headerNameLen > len(data) {
			break
		}
		headerName := string(data[offset : offset+headerNameLen])
		offset += headerNameLen

		if offset+1 > len(data) {
			break
		}
		headerType := data[offset]
		offset++

		var headerValue string
		switch headerType {
		case eventStreamHeaderTypeID:
			if offset+2 > len(data) {
				goto done
			}
			valLen := int(binary.BigEndian.Uint16(data[offset : offset+2]))
			offset += 2
			if offset+valLen > len(data) {
				goto done
			}
			headerValue = string(data[offset : offset+valLen])
			offset += valLen

		case eventStreamDataTypeID:
			headerValue = base64.StdEncoding.EncodeToString(data[offset : offset+2])
			offset += 2

		case eventStreamMessageTypeID:
			if offset+1 > len(data) {
				goto done
			}
			valLen := int(data[offset])
			offset++
			if offset+valLen > len(data) {
				goto done
			}
			headerValue = string(data[offset : offset+valLen])
			offset += valLen

		default:
			goto done
		}

		headers[headerName] = headerValue
	}

done:
	return headers
}

func ExtractAssistantContent(events []EventStreamEvent) string {
	var sb strings.Builder
	for _, event := range events {
		if content := extractAssistantContentPayload(event.Payload); content != "" {
			_, _ = sb.WriteString(content)
		}
	}
	return sb.String()
}

func ExtractToolCalls(events []EventStreamEvent) []OpenAIToolCall {
	collector := newToolCallCollector()
	for _, event := range events {
		if toolEvent := extractToolUsePayload(event.Payload); toolEvent != nil {
			collector.Add(*toolEvent)
		}
	}
	return collector.Finish()
}

func extractAssistantContentPayload(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	var resp AssistantResponseEvent
	if err := json.Unmarshal(payload, &resp); err != nil {
		return ""
	}
	if resp.AssistantResponseEvent.Content != "" {
		return resp.AssistantResponseEvent.Content
	}
	return resp.Content
}

func extractToolUsePayload(payload []byte) *ToolUseEvent {
	if len(payload) == 0 {
		return nil
	}
	var resp ToolUseEvent
	if err := json.Unmarshal(payload, &resp); err != nil {
		return nil
	}
	if resp.ToolUseEvent.ToolUseID != "" || resp.ToolUseEvent.Name != "" || resp.ToolUseEvent.Input != "" || resp.ToolUseEvent.Stop {
		resp.ToolUseID = resp.ToolUseEvent.ToolUseID
		resp.Name = resp.ToolUseEvent.Name
		resp.Input = resp.ToolUseEvent.Input
		resp.Stop = resp.ToolUseEvent.Stop
	}
	if resp.ToolUseID == "" && resp.Name == "" && resp.Input == "" && !resp.Stop {
		return nil
	}
	return &resp
}

type toolCallCollector struct {
	order   []string
	byID    map[string]*OpenAIToolCall
	buffers map[string]*strings.Builder
}

func newToolCallCollector() *toolCallCollector {
	return &toolCallCollector{
		byID:    make(map[string]*OpenAIToolCall),
		buffers: make(map[string]*strings.Builder),
	}
}

type ToolCallCollector struct {
	inner *toolCallCollector
}

func NewToolCallCollectorForStream() *ToolCallCollector {
	return &ToolCallCollector{inner: newToolCallCollector()}
}

func (c *ToolCallCollector) Add(event ToolUseEvent) {
	if c == nil {
		return
	}
	if c.inner == nil {
		c.inner = newToolCallCollector()
	}
	c.inner.Add(event)
}

func (c *ToolCallCollector) Finish() []OpenAIToolCall {
	if c == nil || c.inner == nil {
		return nil
	}
	return c.inner.Finish()
}

func (c *toolCallCollector) Add(event ToolUseEvent) {
	id := strings.TrimSpace(event.ToolUseID)
	if id == "" {
		id = fmt.Sprintf("call_kiro_%d", len(c.order))
	}
	call, exists := c.byID[id]
	if !exists {
		call = &OpenAIToolCall{
			ID:   id,
			Type: "function",
			Function: OpenAIToolFunction{
				Name:      strings.TrimSpace(event.Name),
				Arguments: "",
			},
		}
		c.byID[id] = call
		c.buffers[id] = &strings.Builder{}
		c.order = append(c.order, id)
	}
	if call.Function.Name == "" && strings.TrimSpace(event.Name) != "" {
		call.Function.Name = strings.TrimSpace(event.Name)
	}
	if event.Input != "" {
		_, _ = c.buffers[id].WriteString(event.Input)
	}
	if event.Stop {
		args := strings.TrimSpace(c.buffers[id].String())
		if args == "" {
			args = "{}"
		}
		call.Function.Arguments = normalizeToolArguments(args)
	}
}

func (c *toolCallCollector) Finish() []OpenAIToolCall {
	out := make([]OpenAIToolCall, 0, len(c.order))
	for _, id := range c.order {
		call := c.byID[id]
		if call == nil || call.Function.Name == "" {
			continue
		}
		if call.Function.Arguments == "" {
			args := strings.TrimSpace(c.buffers[id].String())
			if args == "" {
				args = "{}"
			}
			call.Function.Arguments = normalizeToolArguments(args)
		}
		out = append(out, *call)
	}
	return out
}

func normalizeToolArguments(args string) string {
	var parsed any
	if err := json.Unmarshal([]byte(args), &parsed); err != nil {
		return "{}"
	}
	normalized, err := json.Marshal(parsed)
	if err != nil {
		return "{}"
	}
	return string(normalized)
}

func ExtractStreamingEvents(reader io.Reader) (<-chan ParsedEvent, <-chan error) {
	eventCh := make(chan ParsedEvent, 64)
	errCh := make(chan error, 1)

	go func() {
		defer close(eventCh)
		defer close(errCh)

		buf := make([]byte, 4096)
		var buffer bytes.Buffer

		for {
			n, err := reader.Read(buf)
			if n > 0 {
				_, _ = buffer.Write(buf[:n])

				for {
					data := buffer.Bytes()
					if len(data) < 12 {
						break
					}

					totalLen := int(binary.BigEndian.Uint32(data[0:4]))
					headersLen := int(binary.BigEndian.Uint32(data[4:8]))
					if totalLen < 16 || totalLen > len(data) {
						break
					}
					if headersLen < 0 || headersLen > totalLen-16 {
						break
					}

					msg := data[:totalLen]
					buffer.Next(totalLen)

					if len(msg) < 16 {
						continue
					}
					headersStart := 12
					headersEnd := headersStart + headersLen
					payload := msg[headersEnd : len(msg)-4]
					if len(payload) == 0 {
						continue
					}

					if content := extractAssistantContentPayload(payload); content != "" {
						eventCh <- ParsedEvent{Content: content}
					}
					if toolEvent := extractToolUsePayload(payload); toolEvent != nil {
						eventCh <- ParsedEvent{ToolUse: toolEvent}
					}
				}
			}

			if err != nil {
				if err != io.EOF {
					errCh <- err
				}
				return
			}
		}
	}()

	return eventCh, errCh
}

func ExtractStreamingChunks(reader io.Reader) (<-chan string, <-chan error) {
	contentCh := make(chan string, 64)
	eventCh, errCh := ExtractStreamingEvents(reader)
	go func() {
		defer close(contentCh)
		for event := range eventCh {
			if event.Content != "" {
				contentCh <- event.Content
			}
		}
	}()
	return contentCh, errCh
}
