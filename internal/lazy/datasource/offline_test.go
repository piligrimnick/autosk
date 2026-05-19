package datasource_test

import (
	"context"
	"path/filepath"
	"testing"

	"autosk/internal/lazy/datasource"
	"autosk/internal/store"
	"autosk/internal/store/doltlite"
)

func newOfflineFx(t *testing.T) (*datasource.Offline, *doltlite.Store, func()) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	ts := doltlite.New()
	if err := ts.Open(ctx, filepath.Join(dir, "test.db")); err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := ts.Migrate(ctx); err != nil {
		_ = ts.Close()
		t.Fatalf("migrate: %v", err)
	}
	ds, err := datasource.NewOffline(ts, dir, nil)
	if err != nil {
		_ = ts.Close()
		t.Fatalf("offline: %v", err)
	}
	return ds, ts, func() { _ = ts.Close() }
}

func TestOffline_CreateAndListTasks(t *testing.T) {
	ctx := context.Background()
	ds, _, closeFn := newOfflineFx(t)
	defer closeFn()

	id, err := ds.CreateTask(ctx, "Refactor token validator", "", 1)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	tasks, err := ds.Tasks(ctx, datasource.DefaultTaskFilter())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tasks) != 1 || tasks[0].ID != id {
		t.Fatalf("got %d tasks, want 1; ids=%v", len(tasks), tasks)
	}
	if tasks[0].Title != "Refactor token validator" {
		t.Fatalf("title=%q", tasks[0].Title)
	}
	if tasks[0].Status != store.StatusNew {
		t.Fatalf("status=%s", tasks[0].Status)
	}
}

func TestOffline_FilterByPriority(t *testing.T) {
	ctx := context.Background()
	ds, _, closeFn := newOfflineFx(t)
	defer closeFn()
	_, _ = ds.CreateTask(ctx, "lo", "", 3)
	_, _ = ds.CreateTask(ctx, "hi", "", 0)
	p := 0
	tasks, err := ds.Tasks(ctx, datasource.TaskFilter{Statuses: store.OpenStatuses(), Priority: &p})
	if err != nil {
		t.Fatalf("tasks: %v", err)
	}
	if len(tasks) != 1 || tasks[0].Title != "hi" {
		t.Fatalf("p0 returned %v", tasks)
	}
}

func TestOffline_FilterBySearch(t *testing.T) {
	ctx := context.Background()
	ds, _, closeFn := newOfflineFx(t)
	defer closeFn()
	_, _ = ds.CreateTask(ctx, "auth thing", "", 2)
	_, _ = ds.CreateTask(ctx, "logging thing", "", 2)
	tasks, _ := ds.Tasks(ctx, datasource.TaskFilter{Statuses: store.OpenStatuses(), Search: "AUTH"})
	if len(tasks) != 1 || tasks[0].Title != "auth thing" {
		t.Fatalf("search=AUTH: %v", tasks)
	}
}

func TestOffline_BlockAndUnblock(t *testing.T) {
	ctx := context.Background()
	ds, _, closeFn := newOfflineFx(t)
	defer closeFn()
	a, _ := ds.CreateTask(ctx, "parent", "", 2)
	b, _ := ds.CreateTask(ctx, "child", "", 2)
	if err := ds.Block(ctx, a, b); err != nil {
		t.Fatalf("block: %v", err)
	}
	got, err := ds.GetTask(ctx, a)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.Blocked || len(got.BlockedBy) != 1 || got.BlockedBy[0] != b {
		t.Fatalf("expected blocked by %s, got blocked=%v blockedBy=%v", b, got.Blocked, got.BlockedBy)
	}
	if err := ds.Unblock(ctx, a, b); err != nil {
		t.Fatalf("unblock: %v", err)
	}
	got, _ = ds.GetTask(ctx, a)
	if got.Blocked {
		t.Fatalf("still blocked after unblock")
	}
}

func TestOffline_UpdateStatusAndPriority(t *testing.T) {
	ctx := context.Background()
	ds, _, closeFn := newOfflineFx(t)
	defer closeFn()
	id, _ := ds.CreateTask(ctx, "x", "", 2)
	if err := ds.UpdateStatus(ctx, id, store.StatusDone); err != nil {
		t.Fatalf("update status: %v", err)
	}
	if err := ds.UpdatePriority(ctx, id, 0); err != nil {
		t.Fatalf("update priority: %v", err)
	}
	got, _ := ds.GetTask(ctx, id)
	if got.Status != store.StatusDone || got.Priority != 0 {
		t.Fatalf("got %+v", got)
	}
}

func TestOffline_CommentRoundTrip(t *testing.T) {
	ctx := context.Background()
	ds, _, closeFn := newOfflineFx(t)
	defer closeFn()
	id, _ := ds.CreateTask(ctx, "x", "", 2)
	if err := ds.AddComment(ctx, id, "hello"); err != nil {
		t.Fatalf("add: %v", err)
	}
	cs, err := ds.Comments(ctx, id)
	if err != nil {
		t.Fatalf("list comments: %v", err)
	}
	if len(cs) != 1 || cs[0].Text != "hello" {
		t.Fatalf("comments: %v", cs)
	}
	tk, _ := ds.GetTask(ctx, id)
	if tk.CommentCount != 1 {
		t.Fatalf("comment count = %d", tk.CommentCount)
	}
}

func TestOffline_HealthIsDown(t *testing.T) {
	ctx := context.Background()
	ds, _, closeFn := newOfflineFx(t)
	defer closeFn()
	h, _ := ds.Healthz(ctx)
	if h.Daemon != "down" {
		t.Fatalf("offline must report down, got %q", h.Daemon)
	}
}

func TestOffline_StreamLiveReturnsErrDaemonRequired(t *testing.T) {
	ctx := context.Background()
	ds, _, closeFn := newOfflineFx(t)
	defer closeFn()
	_, err := ds.StreamLive(ctx, "job-xxxx")
	if err != datasource.ErrDaemonRequired {
		t.Fatalf("want ErrDaemonRequired, got %v", err)
	}
}
