// Package transcript reads pi's session.jsonl and projects it to the
// daemon's stable Event shape. See plan §5.4.
//
// Format (per pi 0.74 dist/core/session-manager.d.ts):
//
//	{ "type":"session", "id":..., "timestamp":..., "cwd":..., ... }
//	{ "type":"message", "id":..., "parentId":..., "timestamp":..., "message":{...} }
//	{ "type":"thinking_level_change", ..., "thinkingLevel":"high" }
//	{ "type":"model_change", "provider":..., "modelId":... }
//	{ "type":"compaction", ... }
//	{ "type":"branch_summary", ... }
//	{ "type":"custom"|"custom_message"|"label"|"session_info", ... }
//
// We do not enforce that pi's schema is exhaustive: unknown entry types
// are projected to Kind=="other" with the raw object preserved.
package transcript

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// Kind is the daemon's normalised event discriminator.
type Kind string

const (
	KindSession           Kind = "session"
	KindUserText          Kind = "user_text"
	KindAssistantText     Kind = "assistant_text"
	KindAssistantThinking Kind = "assistant_thinking"
	KindToolCall          Kind = "tool_call"
	KindToolResult        Kind = "tool_result"
	KindThinkingLevel     Kind = "thinking_level_change"
	KindModelChange       Kind = "model_change"
	KindCompaction        Kind = "compaction"
	KindBranchSummary     Kind = "branch_summary"
	KindLabel             Kind = "label"
	KindSessionInfo       Kind = "session_info"
	KindCustom            Kind = "custom"
	KindCustomMessage     Kind = "custom_message"
	KindOther             Kind = "other"
)

// Event is the projected shape served by the messages API.
type Event struct {
	Kind    Kind            `json:"kind"`
	TS      time.Time       `json:"ts,omitempty"`
	Text    string          `json:"text,omitempty"`
	Name    string          `json:"name,omitempty"`     // tool name for KindToolCall/KindToolResult
	Input   json.RawMessage `json:"input,omitempty"`    // tool args
	IsError bool            `json:"is_error,omitempty"` // tool_result error flag
	Raw     json.RawMessage `json:"raw"`
}

// ErrMissing wraps os.ErrNotExist when the session file is gone — the
// daemon maps this to HTTP 410 Gone (see plan §11).
var ErrMissing = errors.New("transcript: session file missing")

// Read parses the entire jsonl file and returns projected events in
// on-disk order.
func Read(path string) ([]Event, error) {
	if path == "" {
		return nil, ErrMissing
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrMissing, path)
		}
		return nil, err
	}
	defer f.Close()
	return parseAll(f)
}

// Tail returns the last n projected events from path. n <= 0 means
// "everything".
func Tail(path string, n int) ([]Event, error) {
	all, err := Read(path)
	if err != nil {
		return nil, err
	}
	if n <= 0 || n >= len(all) {
		return all, nil
	}
	return all[len(all)-n:], nil
}

// parseAll reads JSON lines from r, projecting each to one or more Events.
// One SessionMessageEntry may expand to several Events (one assistant
// message can contain multiple toolCalls and text blocks).
func parseAll(r io.Reader) ([]Event, error) {
	dec := json.NewDecoder(r)
	dec.UseNumber()
	var out []Event
	for {
		// Decoder operates per-token; for JSONL we use Decode on a stream.
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			// Skip malformed lines: project as KindOther with the raw byte slice
			// up to the next newline. encoding/json's Decoder buffers; we
			// can't easily peek a single line, so fall through with the
			// error wrapped.
			return out, fmt.Errorf("transcript: decode: %w", err)
		}
		events, err := projectEntry(raw)
		if err != nil {
			out = append(out, Event{Kind: KindOther, Raw: raw})
			continue
		}
		out = append(out, events...)
	}
	return out, nil
}

// projectEntry inspects entry's "type" and returns 0+ projected Events.
func projectEntry(raw json.RawMessage) ([]Event, error) {
	var hdr struct {
		Type      string          `json:"type"`
		Timestamp string          `json:"timestamp"`
		Message   json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal(raw, &hdr); err != nil {
		return nil, err
	}
	ts := parseTimestamp(hdr.Timestamp)

	switch hdr.Type {
	case "session":
		return []Event{{Kind: KindSession, TS: ts, Raw: raw}}, nil
	case "thinking_level_change":
		return []Event{{Kind: KindThinkingLevel, TS: ts, Raw: raw}}, nil
	case "model_change":
		return []Event{{Kind: KindModelChange, TS: ts, Raw: raw}}, nil
	case "compaction":
		return []Event{{Kind: KindCompaction, TS: ts, Raw: raw}}, nil
	case "branch_summary":
		return []Event{{Kind: KindBranchSummary, TS: ts, Raw: raw}}, nil
	case "label":
		return []Event{{Kind: KindLabel, TS: ts, Raw: raw}}, nil
	case "session_info":
		return []Event{{Kind: KindSessionInfo, TS: ts, Raw: raw}}, nil
	case "custom":
		return []Event{{Kind: KindCustom, TS: ts, Raw: raw}}, nil
	case "custom_message":
		return []Event{{Kind: KindCustomMessage, TS: ts, Raw: raw}}, nil
	case "message":
		return projectMessage(hdr.Message, ts, raw), nil
	}
	return []Event{{Kind: KindOther, TS: ts, Raw: raw}}, nil
}

// projectMessage projects an AgentMessage into 0+ events. The shape we
// look at:
//
//	{ "role": "user" | "assistant" | "toolResult", "content": ... }
func projectMessage(msg json.RawMessage, ts time.Time, parentRaw json.RawMessage) []Event {
	var m struct {
		Role       string          `json:"role"`
		ToolCallID string          `json:"toolCallId,omitempty"`
		ToolName   string          `json:"toolName,omitempty"`
		IsError    bool            `json:"isError,omitempty"`
		Content    json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(msg, &m); err != nil {
		return []Event{{Kind: KindOther, TS: ts, Raw: parentRaw}}
	}
	switch m.Role {
	case "user":
		text := flattenTextContent(m.Content)
		return []Event{{Kind: KindUserText, TS: ts, Text: text, Raw: parentRaw}}
	case "assistant":
		return projectAssistantContent(m.Content, ts, parentRaw)
	case "toolResult":
		text := flattenTextContent(m.Content)
		return []Event{{
			Kind:    KindToolResult,
			TS:      ts,
			Name:    m.ToolName,
			Text:    text,
			IsError: m.IsError,
			Raw:     parentRaw,
		}}
	}
	return []Event{{Kind: KindOther, TS: ts, Raw: parentRaw}}
}

// projectAssistantContent walks an assistant message's content array and
// projects each block. Same raw is reused for every emitted event so
// consumers can see the original message.
func projectAssistantContent(content json.RawMessage, ts time.Time, raw json.RawMessage) []Event {
	var blocks []json.RawMessage
	// content can be either a string (legacy) or an array of blocks.
	if err := json.Unmarshal(content, &blocks); err != nil {
		var s string
		if err := json.Unmarshal(content, &s); err == nil {
			return []Event{{Kind: KindAssistantText, TS: ts, Text: s, Raw: raw}}
		}
		return []Event{{Kind: KindOther, TS: ts, Raw: raw}}
	}
	var out []Event
	for _, b := range blocks {
		var head struct {
			Type      string          `json:"type"`
			Text      string          `json:"text"`
			Name      string          `json:"name,omitempty"`
			Arguments json.RawMessage `json:"arguments,omitempty"`
			Thinking  string          `json:"thinking,omitempty"`
		}
		if err := json.Unmarshal(b, &head); err != nil {
			out = append(out, Event{Kind: KindOther, TS: ts, Raw: raw})
			continue
		}
		switch head.Type {
		case "text":
			if strings.TrimSpace(head.Text) == "" {
				continue
			}
			out = append(out, Event{Kind: KindAssistantText, TS: ts, Text: head.Text, Raw: raw})
		case "thinking":
			out = append(out, Event{Kind: KindAssistantThinking, TS: ts, Text: head.Thinking, Raw: raw})
		case "toolCall":
			out = append(out, Event{Kind: KindToolCall, TS: ts, Name: head.Name, Input: head.Arguments, Raw: raw})
		default:
			out = append(out, Event{Kind: KindOther, TS: ts, Raw: raw})
		}
	}
	return out
}

// flattenTextContent collapses an array-of-blocks-or-string content into
// a single text string, joining text blocks with "\n".
func flattenTextContent(content json.RawMessage) string {
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		return s
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(content, &blocks); err != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		var head struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(b, &head); err != nil {
			continue
		}
		if head.Type == "text" && head.Text != "" {
			parts = append(parts, head.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func parseTimestamp(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return time.Time{}
}
