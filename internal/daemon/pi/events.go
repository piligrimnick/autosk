package pi

import (
	"encoding/json"
	"time"
)

// EventKind is a stable, daemon-defined projection of pi's event stream.
// New pi event types degrade to KindOther with the original payload preserved
// in Event.Raw (see plan §5.4).
type EventKind string

const (
	KindAgentStart        EventKind = "agent_start"
	KindAgentEnd          EventKind = "agent_end"
	KindTurnStart         EventKind = "turn_start"
	KindTurnEnd           EventKind = "turn_end"
	KindMessageStart      EventKind = "message_start"
	KindMessageUpdate     EventKind = "message_update"
	KindMessageEnd        EventKind = "message_end"
	KindToolStart         EventKind = "tool_execution_start"
	KindToolUpdate        EventKind = "tool_execution_update"
	KindToolEnd           EventKind = "tool_execution_end"
	KindExtensionRequest  EventKind = "extension_ui_request"
	KindExtensionResponse EventKind = "extension_ui_response"
	KindResponse          EventKind = "response"
	KindOther             EventKind = "other"
)

// Event is the normalised shape that flows on Runner.Events().
//
// Raw is the original pi wire object; consumers (SSE / messages API) emit it
// verbatim. ReceivedAt is stamped by the runner on read.
type Event struct {
	Kind       EventKind       `json:"kind"`
	ReceivedAt time.Time       `json:"received_at"`
	Raw        json.RawMessage `json:"raw"`
}

// classify maps a raw inbound message's discriminator to an EventKind.
func classify(typ string) EventKind {
	switch typ {
	case "agent_start":
		return KindAgentStart
	case "agent_end":
		return KindAgentEnd
	case "turn_start":
		return KindTurnStart
	case "turn_end":
		return KindTurnEnd
	case "message_start":
		return KindMessageStart
	case "message_update":
		return KindMessageUpdate
	case "message_end":
		return KindMessageEnd
	case "tool_execution_start":
		return KindToolStart
	case "tool_execution_update":
		return KindToolUpdate
	case "tool_execution_end":
		return KindToolEnd
	case "extension_ui_request":
		return KindExtensionRequest
	case "extension_ui_response":
		return KindExtensionResponse
	case "response":
		return KindResponse
	default:
		return KindOther
	}
}
