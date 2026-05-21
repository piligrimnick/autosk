// Package pi spawns and communicates with `pi --mode rpc` child processes.
//
// Protocol: JSON Lines on both stdin and stdout. The wire shapes mirror
// pi 0.74's `RpcCommand`, `RpcResponse`, `RpcExtensionUI*` and AgentEvent
// (see docs/notes/pi-rpc-contract.md).
package pi

import "encoding/json"

// Command is one outgoing JSON-line message on pi's stdin.
//
// We expose only the subset the daemon uses (see contract doc §"Commands
// we send"). Unused fields are pointers so they get omitted via omitempty.
type Command struct {
	ID                string `json:"id,omitempty"`
	Type              string `json:"type"`
	Message           string `json:"message,omitempty"`
	StreamingBehavior string `json:"streamingBehavior,omitempty"`
	Provider          string `json:"provider,omitempty"`
	ModelID           string `json:"modelId,omitempty"`
	Level             string `json:"level,omitempty"`
}

// inboundMessage is a generic inbound JSON line — we peek `type` and `id`
// to route, then unmarshal into a more specific struct.
type inboundMessage struct {
	Type    string          `json:"type"`
	ID      string          `json:"id,omitempty"`
	Command string          `json:"command,omitempty"`
	Method  string          `json:"method,omitempty"`
	Success *bool           `json:"success,omitempty"`
	Error   string          `json:"error,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
	Raw     json.RawMessage `json:"-"` // populated by the reader
}

// Response is the parsed form of a `{type:"response", ...}` line.
type Response struct {
	ID      string
	Command string
	Success bool
	Error   string
	Data    json.RawMessage
}

// SessionInfo is the slice of get_state we care about.
type SessionInfo struct {
	SessionID    string `json:"sessionId"`
	SessionFile  string `json:"sessionFile,omitempty"`
	MessageCount int    `json:"messageCount,omitempty"`
}

// ExtensionUIRequest is the host-facing dialog request.
type ExtensionUIRequest struct {
	ID     string
	Method string
	Raw    json.RawMessage // entire frame, for forwarding to SSE if needed
}

// ExtensionUIResponse is what we send back. Daemon always cancels blocking
// dialogs to keep headless runs from hanging.
type ExtensionUIResponse struct {
	Type      string `json:"type"` // "extension_ui_response"
	ID        string `json:"id"`
	Cancelled bool   `json:"cancelled,omitempty"`
	Value     string `json:"value,omitempty"`
	Confirmed bool   `json:"confirmed,omitempty"`
}
