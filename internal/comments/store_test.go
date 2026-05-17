package comments_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"autosk/internal/agent"
	"autosk/internal/comments"
	"autosk/internal/store"
	"autosk/internal/store/doltlite"
)

type fixture struct {
	cs       *comments.Store
	ts       *doltlite.Store
	taskID   string
	humanID  string
	devID    string
	close    func()
}

func newFx(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	ts := doltlite.New()
	if err := ts.Open(ctx, filepath.Join(dir, "test.db")); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := ts.Migrate(ctx); err != nil {
		_ = ts.Close()
		t.Fatalf("Migrate: %v", err)
	}
	ag := agent.New(ts.DB())
	human, err := ag.GetByName(ctx, "human")
	if err != nil {
		_ = ts.Close()
		t.Fatalf("seeded human: %v", err)
	}
	dev, err := ag.Create(ctx, "developer", false)
	if err != nil {
		_ = ts.Close()
		t.Fatalf("dev: %v", err)
	}
	tk, err := ts.CreateTask(ctx, store.Task{Title: "x", Status: store.StatusNew, Priority: 2})
	if err != nil {
		_ = ts.Close()
		t.Fatalf("task: %v", err)
	}
	return &fixture{
		cs:      comments.New(ts.DB()),
		ts:      ts,
		taskID:  tk.ID,
		humanID: human.ID,
		devID:   dev.ID,
		close:   func() { _ = ts.Close() },
	}
}

func TestAdd_Roundtrip(t *testing.T) {
	fx := newFx(t)
	defer fx.close()
	ctx := context.Background()
	c, err := fx.cs.Add(ctx, fx.taskID, fx.humanID, "hello")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if c.ID == 0 || c.Text != "hello" || c.AuthorName != "human" {
		t.Fatalf("shape: %+v", c)
	}
	if time.Since(c.CreatedAt) > 2*time.Second {
		t.Errorf("created_at too far in past: %v", c.CreatedAt)
	}
}

func TestAdd_RejectsEmpty(t *testing.T) {
	fx := newFx(t)
	defer fx.close()
	for _, txt := range []string{"", "   ", "\n\n"} {
		_, err := fx.cs.Add(context.Background(), fx.taskID, fx.humanID, txt)
		if !errors.Is(err, comments.ErrEmptyText) {
			t.Errorf("text=%q want ErrEmptyText got %v", txt, err)
		}
	}
}

func TestListByTask_OldestFirst(t *testing.T) {
	fx := newFx(t)
	defer fx.close()
	ctx := context.Background()
	if _, err := fx.cs.Add(ctx, fx.taskID, fx.humanID, "first"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond) // sub-second timestamps won't differ
	if _, err := fx.cs.Add(ctx, fx.taskID, fx.devID, "second"); err != nil {
		t.Fatal(err)
	}
	got, err := fx.cs.ListByTask(ctx, fx.taskID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("count=%d", len(got))
	}
	if got[0].Text != "first" || got[1].Text != "second" {
		t.Fatalf("order: %v / %v", got[0].Text, got[1].Text)
	}
	if got[0].AuthorName != "human" || got[1].AuthorName != "developer" {
		t.Fatalf("authors: %v / %v", got[0].AuthorName, got[1].AuthorName)
	}
}

func TestRenderForPrompt(t *testing.T) {
	fx := newFx(t)
	defer fx.close()
	ctx := context.Background()
	if _, err := fx.cs.Add(ctx, fx.taskID, fx.devID, "looks good"); err != nil {
		t.Fatal(err)
	}
	lines, err := fx.cs.RenderForPrompt(ctx, fx.taskID)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 {
		t.Fatalf("lines=%d", len(lines))
	}
	if !strings.HasPrefix(lines[0], "[developer@") || !strings.HasSuffix(lines[0], "]: looks good") {
		t.Fatalf("format: %q", lines[0])
	}
}

func TestListByTask_DeleteCascade(t *testing.T) {
	fx := newFx(t)
	defer fx.close()
	ctx := context.Background()
	if _, err := fx.cs.Add(ctx, fx.taskID, fx.humanID, "doomed"); err != nil {
		t.Fatal(err)
	}
	// Deleting the task should cascade the comment row away.
	if _, err := fx.ts.DB().ExecContext(ctx, `DELETE FROM tasks WHERE id = ?`, fx.taskID); err != nil {
		t.Fatal(err)
	}
	got, err := fx.cs.ListByTask(ctx, fx.taskID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("expected cascade, got %d rows", len(got))
	}
}
