// Package meta provides typed accessors for the reserved sections of
// the per-task `metadata` JSON blob (tasks.metadata).
//
// Most of metadata is free-form, but the engine reserves a small set of
// top-level keys for its own use. Today there is exactly one: the
// `step_visits` map, which tracks how many times a task has entered
// each workflow step (see docs/plans/20260520-Step-Visit-Limits.md).
//
// Storing reserved data through these helpers (instead of indexing the
// raw map by string) keeps the engine from accidentally corrupting the
// shape after a json.Unmarshal round-trip widens int → float64.
package meta

import "fmt"

// StepVisitsKey is the reserved top-level metadata key whose value is a
// map step_id → visit count (>=0). Other tools should treat anything
// under this key as engine-owned.
const StepVisitsKey = "step_visits"

// StepVisits is the typed shape of metadata[StepVisitsKey]. Keys are
// step ids ("st-XXXX"); values are visit counts.
type StepVisits map[string]int

// GetStepVisits returns a typed copy of the reserved step_visits map.
// Tolerates the wire shape produced by json.Unmarshal — integer values
// are decoded as float64 by default. Unknown / malformed values are
// dropped silently to keep the engine resilient to free-form edits.
//
// Returns a non-nil, possibly empty map.
func GetStepVisits(m map[string]any) StepVisits {
	out := make(StepVisits)
	if m == nil {
		return out
	}
	raw, ok := m[StepVisitsKey]
	if !ok || raw == nil {
		return out
	}
	rawMap, ok := raw.(map[string]any)
	if !ok {
		// Some other tool wrote a non-object under step_visits. Treat as
		// "no recorded visits"; the engine never reads it that way and
		// MutateStepVisits below will overwrite it on the next bump.
		return out
	}
	for k, v := range rawMap {
		switch n := v.(type) {
		case float64:
			out[k] = int(n)
		case int:
			out[k] = n
		case int64:
			out[k] = int(n)
		}
	}
	return out
}

// SetStepVisits replaces the reserved step_visits sub-object on m with
// sv. The map is stored as `map[string]any` so json.Marshal produces
// the canonical wire shape; integer values round-trip correctly.
//
// Mutates m in place. Panics if m is nil. The store.Store contract
// promises every UpdateMetadata closure receives a non-nil map (see
// store.Store.UpdateMetadata docs), so the typical engine pattern
// "load, MutateStepVisits, save" cannot trip this.
func SetStepVisits(m map[string]any, sv StepVisits) {
	if m == nil {
		panic("meta.SetStepVisits: nil map")
	}
	if len(sv) == 0 {
		delete(m, StepVisitsKey)
		return
	}
	out := make(map[string]any, len(sv))
	for k, v := range sv {
		out[k] = v
	}
	m[StepVisitsKey] = out
}

// MutateStepVisits is the read-modify-write convenience used by the
// engine and the CLI. It hands fn a typed StepVisits, then writes the
// result back. An empty resulting map deletes the reserved key entirely
// so an empty metadata blob round-trips back to NULL.
//
// Panics if m is nil. The store.Store.UpdateMetadata contract
// guarantees the closure receives a non-nil map; this is what makes
// the panic unreachable from real engine code.
func MutateStepVisits(m map[string]any, fn func(StepVisits)) {
	if m == nil {
		panic("meta.MutateStepVisits: nil map")
	}
	sv := GetStepVisits(m)
	fn(sv)
	SetStepVisits(m, sv)
}

// ValidateStepVisitsLeaf returns nil iff v is a non-negative integer
// (or its float64 wire shape). Used by `autosk metadata set` to guard
// humans from corrupting the engine's view by writing a non-int leaf
// under step_visits.
func ValidateStepVisitsLeaf(v any) error {
	switch n := v.(type) {
	case int:
		if n < 0 {
			return fmt.Errorf("step_visits leaves must be >= 0, got %d", n)
		}
		return nil
	case int64:
		if n < 0 {
			return fmt.Errorf("step_visits leaves must be >= 0, got %d", n)
		}
		return nil
	case float64:
		if n < 0 || n != float64(int(n)) {
			return fmt.Errorf("step_visits leaves must be non-negative integers, got %v", v)
		}
		return nil
	default:
		return fmt.Errorf("step_visits leaves must be integers, got %T", v)
	}
}

// ValidateStepVisitsObject returns nil iff v is a JSON object whose
// every leaf passes ValidateStepVisitsLeaf. The CLI uses this when a
// human writes `metadata set --key step_visits --value '{...}'`.
func ValidateStepVisitsObject(v any) error {
	if v == nil {
		return nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return fmt.Errorf("step_visits must be a JSON object, got %T", v)
	}
	for k, leaf := range m {
		if err := ValidateStepVisitsLeaf(leaf); err != nil {
			return fmt.Errorf("step_visits[%q]: %w", k, err)
		}
	}
	return nil
}
