//go:build doltserver

// Package doltserver is a placeholder for the future external-dolt-sql-server
// backend. Built only when the `doltserver` build tag is set; the default
// build sees the disabled.go stub instead.
//
// Every method returns ErrNotImplemented. This file exists so the Store
// interface stays under compile-time pressure as it evolves.
package doltserver

import (
	"context"

	"autosk/internal/store"
)

// Available reports whether this build supports the dolt-server backend.
const Available = true

// Store is the dolt-server implementation. Currently a stub.
type Store struct{}

var _ store.Store = (*Store)(nil)

// New returns an unopened Store.
func New() *Store { return &Store{} }

func (s *Store) Open(ctx context.Context, dbPath string) error { return store.ErrNotImplemented }
func (s *Store) Close() error                                  { return nil }
func (s *Store) Migrate(ctx context.Context) error             { return store.ErrNotImplemented }
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	return 0, store.ErrNotImplemented
}

func (s *Store) CreateTask(ctx context.Context, t store.Task) (store.Task, error) {
	return store.Task{}, store.ErrNotImplemented
}
func (s *Store) GetTask(ctx context.Context, id string) (store.Task, error) {
	return store.Task{}, store.ErrNotImplemented
}
func (s *Store) UpdateTask(ctx context.Context, id string, p store.TaskPatch) (store.Task, error) {
	return store.Task{}, store.ErrNotImplemented
}
func (s *Store) DeleteTask(ctx context.Context, id string) error {
	return store.ErrNotImplemented
}
func (s *Store) UpdateMetadata(ctx context.Context, id string, fn func(m map[string]any) error) (map[string]any, bool, error) {
	return nil, false, store.ErrNotImplemented
}
func (s *Store) UpdateMetadataAndPatch(ctx context.Context, id string, fn func(m map[string]any) error, p store.TaskPatch) (store.Task, error) {
	return store.Task{}, store.ErrNotImplemented
}
func (s *Store) ListTasks(ctx context.Context, f store.ListFilter) ([]store.Task, error) {
	return nil, store.ErrNotImplemented
}
func (s *Store) Claim(ctx context.Context, id string) (store.Task, error) {
	return store.Task{}, store.ErrNotImplemented
}
func (s *Store) Block(ctx context.Context, id string, blockers ...string) error {
	return store.ErrNotImplemented
}
func (s *Store) Unblock(ctx context.Context, id string, blockers ...string) error {
	return store.ErrNotImplemented
}
func (s *Store) UnblockAll(ctx context.Context, id string) (int, error) {
	return 0, store.ErrNotImplemented
}
func (s *Store) Deps(ctx context.Context, id string) (incoming, outgoing []string, err error) {
	return nil, nil, store.ErrNotImplemented
}
func (s *Store) IsBlocked(ctx context.Context, id string) (bool, error) {
	return false, store.ErrNotImplemented
}
func (s *Store) Ready(ctx context.Context, limit int) ([]store.Task, error) {
	return nil, store.ErrNotImplemented
}
func (s *Store) QueryRaw(ctx context.Context, q string, args ...any) (store.Rows, error) {
	return nil, store.ErrNotImplemented
}
func (s *Store) ExecRaw(ctx context.Context, q string, args ...any) (store.Result, error) {
	return nil, store.ErrNotImplemented
}
