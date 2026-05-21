// Package conformance contains a shared test suite every backend must pass.
//
// A backend's test file invokes RunConformance with a factory that returns
// a freshly-opened, migrated store backed by a temp directory. The suite
// grows as phases land:
//
//   - P1: Open / Close / Migrate / SchemaVersion (this file).
//   - P2: CreateTask / GetTask basics.
//   - P3: ListTasks / Ready (v0.2: no Claim).
//   - P4: Block / Unblock / cycle detection / edge-aware Ready.
package conformance

import (
	"context"
	"errors"
	"testing"

	"autosk/internal/store"
)

// Factory returns an opened, migrated store and a cleanup func.
// The cleanup func MUST close the store and remove any temp files.
type Factory func(t *testing.T) (s store.Store, cleanup func())

// RunConformance runs every applicable suite against the factory.
func RunConformance(t *testing.T, f Factory) {
	t.Helper()
	t.Run("StatusLengths", AssertStatusLengths)
	t.Run("Lifecycle", func(t *testing.T) { testLifecycle(t, f) })
	t.Run("MigrateIdempotent", func(t *testing.T) { testMigrateIdempotent(t, f) })
	t.Run("RawPassthrough", func(t *testing.T) { testRawPassthrough(t, f) })
	t.Run("CreateGetRoundtrip", func(t *testing.T) { testCreateGetRoundtrip(t, f) })
	t.Run("CreateRejectsEmptyTitle", func(t *testing.T) { testCreateRejectsEmptyTitle(t, f) })
	t.Run("CreateRejectsBadPriority", func(t *testing.T) { testCreateRejectsBadPriority(t, f) })
	t.Run("GetMissingReturnsNotFound", func(t *testing.T) { testGetMissingReturnsNotFound(t, f) })
	t.Run("CreateAutoAssignsID", func(t *testing.T) { testCreateAutoAssignsID(t, f) })

	t.Run("DeleteTaskRemovesRow", func(t *testing.T) { testDeleteTaskRemovesRow(t, f) })
	t.Run("DeleteTaskMissingReturnsNotFound", func(t *testing.T) { testDeleteTaskMissingReturnsNotFound(t, f) })
	t.Run("DeleteTaskCascadesDepsAndComments", func(t *testing.T) { testDeleteTaskCascadesDepsAndComments(t, f) })

	t.Run("ListDefaultsToOpen", func(t *testing.T) { testListDefaultsToOpen(t, f) })
	t.Run("ListAllShowsTerminal", func(t *testing.T) { testListAllShowsTerminal(t, f) })
	t.Run("ListSorting", func(t *testing.T) { testListSorting(t, f) })
	t.Run("UpdatePartial", func(t *testing.T) { testUpdatePartial(t, f) })
	t.Run("UpdateRejectsBadStatus", func(t *testing.T) { testUpdateRejectsBadStatus(t, f) })
	t.Run("ReadyExcludesDone", func(t *testing.T) { testReadyExcludesDone(t, f) })
	t.Run("V02_HumanAgentSeeded", func(t *testing.T) { testHumanAgentSeeded(t, f) })
	t.Run("V02_TablesPresent", func(t *testing.T) { testV02TablesPresent(t, f) })

	t.Run("BlockBasics", func(t *testing.T) { testBlockBasics(t, f) })
	t.Run("BlockRejectsSelfBlock", func(t *testing.T) { testBlockRejectsSelfBlock(t, f) })
	t.Run("BlockRejectsDirectCycle", func(t *testing.T) { testBlockRejectsDirectCycle(t, f) })
	t.Run("BlockRejectsTransitiveCycle", func(t *testing.T) { testBlockRejectsTransitiveCycle(t, f) })
	t.Run("BlockIdempotent", func(t *testing.T) { testBlockIdempotent(t, f) })
	t.Run("UnblockSpecificEdge", func(t *testing.T) { testUnblockSpecificEdge(t, f) })
	t.Run("UnblockAllRemovesIncoming", func(t *testing.T) { testUnblockAllRemovesIncoming(t, f) })
	t.Run("ReadyExcludesBlockedByOpen", func(t *testing.T) { testReadyExcludesBlockedByOpen(t, f) })
	t.Run("CancelledBlockerDoesNotBlock", func(t *testing.T) { testCancelledBlockerDoesNotBlock(t, f) })
	t.Run("IsBlocked", func(t *testing.T) { testIsBlocked(t, f) })
}

func testLifecycle(t *testing.T, f Factory) {
	s, cleanup := f(t)
	defer cleanup()

	ctx := context.Background()
	v, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if v < 1 {
		t.Fatalf("expected schema_version >= 1, got %d", v)
	}
}

func testMigrateIdempotent(t *testing.T, f Factory) {
	s, cleanup := f(t)
	defer cleanup()

	ctx := context.Background()
	v1, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate (2nd): %v", err)
	}
	v2, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaVersion (post): %v", err)
	}
	if v1 != v2 {
		t.Fatalf("Migrate is not idempotent: %d → %d", v1, v2)
	}
}

func testRawPassthrough(t *testing.T, f Factory) {
	s, cleanup := f(t)
	defer cleanup()

	ctx := context.Background()
	rows, err := s.QueryRaw(ctx, `SELECT name FROM sqlite_master WHERE type='table' ORDER BY name`)
	if err != nil {
		t.Fatalf("QueryRaw: %v", err)
	}
	defer rows.Close()

	got := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		got[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	for _, want := range []string{
		"agents", "workflows", "steps", "step_transitions",
		"tasks", "task_deps", "comments",
		"daemon_runs", "step_signals",
		"schema_migrations",
	} {
		if !got[want] {
			t.Errorf("expected table %q to exist after Migrate; got: %v", want, got)
		}
	}
}

// testHumanAgentSeeded verifies migrations.SeedHumanAgent ran.
func testHumanAgentSeeded(t *testing.T, f Factory) {
	s, cleanup := f(t)
	defer cleanup()
	ctx := context.Background()
	rows, err := s.QueryRaw(ctx, `SELECT name, is_human FROM agents WHERE name='human'`)
	if err != nil {
		t.Fatalf("QueryRaw agents: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("expected a row in agents with name='human' after migration")
	}
	var name string
	var isHuman int
	if err := rows.Scan(&name, &isHuman); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if name != "human" || isHuman != 1 {
		t.Fatalf("unexpected human agent row: name=%q is_human=%d", name, isHuman)
	}
}

// testV02TablesPresent re-uses testRawPassthrough but exists under a
// stable name so future suites can assert it from their own t.Runs.
func testV02TablesPresent(t *testing.T, f Factory) {
	testRawPassthrough(t, f)
}

// AssertErrIs is a small helper used by per-phase suites.
func AssertErrIs(t *testing.T, got, want error) {
	t.Helper()
	if !errors.Is(got, want) {
		t.Fatalf("want %v, got %v", want, got)
	}
}

// ---- P2 task CRUD --------------------------------------------------------

func testCreateGetRoundtrip(t *testing.T, f Factory) {
	s, cleanup := f(t)
	defer cleanup()
	ctx := context.Background()

	in := store.Task{
		Title:       "do the thing",
		Description: "with feeling",
		Status:      store.StatusNew,
		Priority:    1,
	}
	created, err := s.CreateTask(ctx, in)
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if created.ID == "" {
		t.Fatal("expected auto-assigned id")
	}
	if created.CreatedAt.IsZero() || created.UpdatedAt.IsZero() {
		t.Fatal("timestamps not set")
	}

	got, err := s.GetTask(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Title != in.Title || got.Description != in.Description ||
		got.Status != in.Status || got.Priority != in.Priority {
		t.Fatalf("roundtrip mismatch:\n in:  %+v\n got: %+v", in, got)
	}
}

func testCreateRejectsEmptyTitle(t *testing.T, f Factory) {
	s, cleanup := f(t)
	defer cleanup()
	_, err := s.CreateTask(context.Background(), store.Task{Title: "   ", Status: store.StatusNew, Priority: 2})
	AssertErrIs(t, err, store.ErrEmptyTitle)
}

func testCreateRejectsBadPriority(t *testing.T, f Factory) {
	s, cleanup := f(t)
	defer cleanup()
	_, err := s.CreateTask(context.Background(), store.Task{Title: "x", Status: store.StatusNew, Priority: 99})
	AssertErrIs(t, err, store.ErrInvalidPriority)
}

func testGetMissingReturnsNotFound(t *testing.T, f Factory) {
	s, cleanup := f(t)
	defer cleanup()
	_, err := s.GetTask(context.Background(), "ask-ffffff")
	AssertErrIs(t, err, store.ErrNotFound)
}

func testCreateAutoAssignsID(t *testing.T, f Factory) {
	s, cleanup := f(t)
	defer cleanup()
	a, err := s.CreateTask(context.Background(), store.Task{Title: "a", Status: store.StatusNew, Priority: 2})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateTask(context.Background(), store.Task{Title: "b", Status: store.StatusNew, Priority: 2})
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == b.ID {
		t.Fatalf("ids collided: %s == %s", a.ID, b.ID)
	}
}
