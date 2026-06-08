// Package agent holds the agent view type shared across the Go CLI +
// lazy TUI.
//
// In v0.2 (see docs/plans/20260517-Workflows-Plan.md §2.1) an agent is
// a minimal row: id + name + is_human + created_at. The executable
// configuration (model, thinking, first-turn prompt, extra args) lives
// in npm packages installed into the autosk packages prefix (see
// internal/agent/pkgregistry and docs/plans/20260518-Agent-Packages.md).
//
// The `agents` table itself is owned by the Rust daemon (autosk-core);
// the Go front ends only ever consume Agent values that arrive over
// JSON-RPC from autoskd.
package agent

import "time"

// IDPrefix is the prefix for agent ids ("ag-XXXX").
const IDPrefix = "ag"

// HumanAgentName is the reserved name of the seeded human agent.
const HumanAgentName = "human"

// Agent is the in-memory representation of an `agents` row.
type Agent struct {
	ID        string
	Name      string
	IsHuman   bool
	CreatedAt time.Time
}
