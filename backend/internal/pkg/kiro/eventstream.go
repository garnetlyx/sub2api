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
	eventStreamHeaderTypeID   = 0x0
	eventStreamDataTypeID     = 0x05
	eventStreamMessageTypeID  = 0x04

	awsEventStreamVersion     = 0x80
	awsEventStreamVersionMask = 0xF0
)

type EventStreamEvent struct {
	Headers map[string]string
	Payload []byte
}

type AssistantResponseEvent struct {
	AssistEventID string `json:"assistEventId"`
	Content       string `json:"content"`
	ModelID       string `json:"modelId"`
	AssistantResponseEvent struct {
		Content string `json:"content"`
	} `json:"assistantResponseEvent"`
}

type ToolUseEvent struct {
	ToolUseEvent struct {
		ToolUseID   string `json:"toolUseId"`
		Name        string `json:"name"`
		Input       string `json:"input"`
	} `json:"toolUseEvent"`
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
			sb.WriteString(content)
		}
	}
	return sb.String()
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

func ExtractStreamingChunks(reader io.Reader) (<-chan string, <-chan error) {
	contentCh := make(chan string, 64)
	errCh := make(chan error, 1)

	go func() {
		defer close(contentCh)
		defer close(errCh)

		buf := make([]byte, 4096)
		var buffer bytes.Buffer

		for {
			n, err := reader.Read(buf)
			if n > 0 {
				buffer.Write(buf[:n])

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
						contentCh <- content
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

	return contentCh, errCh
}
