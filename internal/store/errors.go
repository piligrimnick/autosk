package store

import "errors"

// Sentinel errors. Use errors.Is to test.
var (
	// ErrNotFound — the requested task does not exist.
	ErrNotFound = errors.New("task not found")

	// ErrNotClaimable — the task is in a terminal status (done or cancel).
	ErrNotClaimable = errors.New("task is not in a claimable state")

	// ErrSelfBlock — a task cannot block itself.
	ErrSelfBlock = errors.New("a task cannot block itself")

	// ErrCycle — adding this edge would close a cycle in the blocker graph.
	ErrCycle = errors.New("edge would create a cycle")

	// ErrBlockerNotFound — one of the blocker ids does not exist.
	ErrBlockerNotFound = errors.New("blocker task not found")

	// ErrInvalidStatus — the proposed Status is not in the enum.
	ErrInvalidStatus = errors.New("invalid status value")

	// ErrInvalidPriority — priority is outside MinPriority..MaxPriority.
	ErrInvalidPriority = errors.New("priority must be in 0..3")

	// ErrEmptyTitle — title is required and may not be empty.
	ErrEmptyTitle = errors.New("title may not be empty")

	// ErrNotImplemented — the backend does not implement this method (yet).
	ErrNotImplemented = errors.New("not implemented")

	// ErrNotOpen — operation called before Open or after Close.
	ErrNotOpen = errors.New("store is not open")

	// ErrInvalidShape — the caller-supplied identifier does not match
	// the canonical shape required by the store (e.g. task ids must be
	// `ask-` + 6 lowercase hex chars).
	ErrInvalidShape = errors.New("invalid id shape")
)
