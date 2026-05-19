package datasource_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"autosk/internal/daemon/client"
	"autosk/internal/lazy/datasource"
	"autosk/internal/store/doltlite"
)

// TestCompose_OfflineWhenDaemonDown verifies that with a non-existent
// UDS path the composer reports daemon=down and routes Jobs (and any
// other read) to the offline source.
func TestCompose_OfflineWhenDaemonDown(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	ts := doltlite.New()
	if err := ts.Open(ctx, filepath.Join(dir, "test.db")); err != nil {
		t.Fatalf("open: %v", err)
	}
	defer ts.Close()
	if err := ts.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	off, err := datasource.NewOffline(ts, dir, nil)
	if err != nil {
		t.Fatalf("offline: %v", err)
	}
	cli, err := client.New(client.Options{Sock: filepath.Join(dir, "no.sock"), Cwd: dir})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	c := datasource.NewCompose(off, cli, 50*time.Millisecond)
	defer c.Close()

	h := c.Health()
	if h.Daemon != "down" {
		t.Fatalf("expected daemon=down, got %q", h.Daemon)
	}
	if _, err := c.Tasks(ctx, datasource.DefaultTaskFilter()); err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	if _, err := c.Jobs(ctx, datasource.JobFilter{}); err != nil {
		t.Fatalf("Jobs: %v", err)
	}
	if err := c.CancelJob(ctx, "job-xxxx"); err != datasource.ErrDaemonRequired {
		t.Fatalf("CancelJob offline: want ErrDaemonRequired, got %v", err)
	}
}
