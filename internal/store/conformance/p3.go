package conformance

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"autosk/internal/store"
)

func mustCreate(t *testing.T, s store.Store, title string, prio int) store.Task {
	t.Helper()
	x, err := s.CreateTask(context.Background(), store.Task{
		Title: title, Status: store.StatusNew, Priority: prio,
	})
	if err != nil {
		t.Fatalf("CreateTask(%q): %v", title, err)
	}
	return x
}

func testListDefaultsToOpen(t *testing.T, f Factory) {
	s, cleanup := f(t)
	defer cleanup()
	ctx := context.Background()

	a := mustCreate(t, s, "a", 1)
	b := mustCreate(t, s, "b", 1)
	c := mustCreate(t, s, "c", 1)
	// Move b → done, c → cancelled.
	target := store.StatusDone
	if _, err := s.UpdateTask(ctx, b.ID, store.TaskPatch{Status: &target}); err != nil {
		t.Fatal(err)
	}
	target = store.StatusCancelled
	if _, err := s.UpdateTask(ctx, c.ID, store.TaskPatch{Status: &target}); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListTasks(ctx, store.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != a.ID {
		t.Fatalf("default list should hide done/cancelled; got %v", taskIDs(got))
	}
}

func testListAllShowsTerminal(t *testing.T, f Factory) {
	s, cleanup := f(t)
	defer cleanup()
	ctx := context.Background()

	mustCreate(t, s, "a", 1)
	b := mustCreate(t, s, "b", 1)
	tg := store.StatusDone
	if _, err := s.UpdateTask(ctx, b.ID, store.TaskPatch{Status: &tg}); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListTasks(ctx, store.ListFilter{Statuses: []store.Status{}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("Statuses=[] should return all, got %d (%v)", len(got), taskIDs(got))
	}
}

func testListSorting(t *testing.T, f Factory) {
	s, cleanup := f(t)
	defer cleanup()
	// Create in non-sorted order.
	p2 := mustCreate(t, s, "p2", 2)
	p0 := mustCreate(t, s, "p0", 0)
	p1 := mustCreate(t, s, "p1", 1)

	got, err := s.ListTasks(context.Background(), store.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{p0.ID, p1.ID, p2.ID}
	if diff := diffIDs(want, taskIDs(got)); diff != "" {
		t.Fatalf("list sort mismatch: %s", diff)
	}
}

func testUpdatePartial(t *testing.T, f Factory) {
	s, cleanup := f(t)
	defer cleanup()
	ctx := context.Background()
	t1 := mustCreate(t, s, "orig", 2)

	newTitle := "renamed"
	newPrio := 0
	out, err := s.UpdateTask(ctx, t1.ID, store.TaskPatch{Title: &newTitle, Priority: &newPrio})
	if err != nil {
		t.Fatal(err)
	}
	if out.Title != "renamed" || out.Priority != 0 || out.Status != store.StatusNew {
		t.Fatalf("partial update wrong: %+v", out)
	}
	if !out.UpdatedAt.After(t1.UpdatedAt) && !out.UpdatedAt.Equal(t1.UpdatedAt) {
		t.Fatalf("updated_at went backwards")
	}
}

func testUpdateRejectsBadStatus(t *testing.T, f Factory) {
	s, cleanup := f(t)
	defer cleanup()
	t1 := mustCreate(t, s, "x", 2)
	bad := store.Status("frobnicated")
	_, err := s.UpdateTask(context.Background(), t1.ID, store.TaskPatch{Status: &bad})
	AssertErrIs(t, err, store.ErrInvalidStatus)
}

func testClaimNewToClaimed(t *testing.T, f Factory) {
	s, cleanup := f(t)
	defer cleanup()
	t1 := mustCreate(t, s, "x", 2)
	got, err := s.Claim(context.Background(), t1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.StatusClaimed {
		t.Fatalf("want claimed, got %s", got.Status)
	}
}

func testClaimIdempotent(t *testing.T, f Factory) {
	s, cleanup := f(t)
	defer cleanup()
	ctx := context.Background()
	t1 := mustCreate(t, s, "x", 2)
	if _, err := s.Claim(ctx, t1.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.Claim(ctx, t1.ID) // second call
	if err != nil {
		t.Fatalf("second claim should be idempotent, got: %v", err)
	}
	if got.Status != store.StatusClaimed {
		t.Fatalf("want claimed after idempotent claim, got %s", got.Status)
	}
}

func testClaimRejectsTerminal(t *testing.T, f Factory) {
	s, cleanup := f(t)
	defer cleanup()
	ctx := context.Background()
	t1 := mustCreate(t, s, "x", 2)
	tg := store.StatusDone
	if _, err := s.UpdateTask(ctx, t1.ID, store.TaskPatch{Status: &tg}); err != nil {
		t.Fatal(err)
	}
	_, err := s.Claim(ctx, t1.ID)
	AssertErrIs(t, err, store.ErrNotClaimable)

	// Same for cancelled.
	t2 := mustCreate(t, s, "y", 2)
	tg = store.StatusCancelled
	if _, err := s.UpdateTask(ctx, t2.ID, store.TaskPatch{Status: &tg}); err != nil {
		t.Fatal(err)
	}
	_, err = s.Claim(ctx, t2.ID)
	AssertErrIs(t, err, store.ErrNotClaimable)
}

// testClaimRace implements acceptance scenario §11.4. N goroutines invoke
// Claim concurrently on the same `new` task. All must succeed (idempotent).
// The post-state must be a single `claimed` row.
func testClaimRace(t *testing.T, f Factory) {
	s, cleanup := f(t)
	defer cleanup()
	ctx := context.Background()
	t1 := mustCreate(t, s, "race", 2)

	const N = 8
	var wg sync.WaitGroup
	var errCount atomic.Int32
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Claim(ctx, t1.ID); err != nil {
				errCount.Add(1)
				t.Errorf("concurrent Claim returned error: %v", err)
			}
		}()
	}
	wg.Wait()
	if errCount.Load() != 0 {
		t.Fatalf("expected zero errors, got %d", errCount.Load())
	}

	post, err := s.GetTask(ctx, t1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if post.Status != store.StatusClaimed {
		t.Fatalf("post-state must be claimed, got %s", post.Status)
	}
}

func testReadyP3NoEdges(t *testing.T, f Factory) {
	s, cleanup := f(t)
	defer cleanup()
	ctx := context.Background()
	mustCreate(t, s, "low", 3)
	hi := mustCreate(t, s, "high", 0)
	// One claimed (not eligible for ready since ready requires status='new').
	c := mustCreate(t, s, "claimed", 0)
	if _, err := s.Claim(ctx, c.ID); err != nil {
		t.Fatal(err)
	}

	got, err := s.Ready(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("ready should return 2 'new' tasks, got %d (%v)", len(got), taskIDs(got))
	}
	if got[0].ID != hi.ID {
		t.Fatalf("ready should be priority-sorted; got order %v", taskIDs(got))
	}

	// Limit honored.
	got, err = s.Ready(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != hi.ID {
		t.Fatalf("limit=1 misbehaved; got %v", taskIDs(got))
	}
}

// ---- helpers --------------------------------------------------------------

func taskIDs(ts []store.Task) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.ID
	}
	return out
}

func diffIDs(want, got []string) string {
	if len(want) != len(got) {
		return "lengths differ"
	}
	for i := range want {
		if want[i] != got[i] {
			return "index " + itoa(i) + ": want " + want[i] + ", got " + got[i]
		}
	}
	return ""
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

