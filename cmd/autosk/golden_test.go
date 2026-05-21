package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"autosk/internal/render"
	"autosk/internal/store"
)

var updateGolden = flag.Bool("update", false, "rewrite golden files")

// Stable inputs so the rendered output is deterministic.
//
// We don't run the binary end-to-end; we exercise the renderer directly with
// fixed inputs. End-to-end tests would re-run cobra + doltlite, which adds
// flakiness around timing and IDs; these goldens nail down the wire shape.

func fixedTask() store.Task {
	return store.Task{
		ID:          "as-a1b2",
		Title:       "Wire up doltlite store",
		Description: "Implement Open/Close/Migrate and the smoke test.",
		Status:      store.StatusWork,
		Priority:    1,
		CreatedAt:   time.Date(2026, 5, 13, 10, 0, 0, 0, time.UTC),
		UpdatedAt:   time.Date(2026, 5, 13, 11, 42, 13, 0, time.UTC),
	}
}

func TestGolden_ShowJSON(t *testing.T) {
	var buf bytes.Buffer
	if err := render.TaskJSONTo(&buf, fixedTask(),
		render.WithBlocked(false, nil, []string{"as-3c4d"})); err != nil {
		t.Fatal(err)
	}
	compareGolden(t, "show.golden.json", buf.Bytes())
}

func TestGolden_ListJSON(t *testing.T) {
	tasks := []store.Task{
		fixedTask(),
		{
			ID: "as-c3d4", Title: "second one", Status: store.StatusNew,
			Priority:  0,
			CreatedAt: time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC),
			UpdatedAt: time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC),
		},
	}
	var buf bytes.Buffer
	if err := render.TasksJSONTo(&buf, tasks, nil); err != nil {
		t.Fatal(err)
	}
	compareGolden(t, "list.golden.json", buf.Bytes())
}

func TestGolden_Empty(t *testing.T) {
	var buf bytes.Buffer
	if err := render.TasksJSONTo(&buf, nil, nil); err != nil {
		t.Fatal(err)
	}
	compareGolden(t, "empty.golden.json", buf.Bytes())
}

// compareGolden compares (or rewrites with -update) a golden file under
// testdata/.
func compareGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("updated %s", path)
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v  (run with -update to create)", path, err)
	}
	if !bytes.Equal(bytes.TrimRight(want, "\n"), bytes.TrimRight(got, "\n")) {
		// Re-emit as canonical JSON for human-readable diff.
		var w any
		_ = json.Unmarshal(want, &w)
		var g any
		_ = json.Unmarshal(got, &g)
		t.Errorf("golden mismatch %s:\n  want: %s\n  got:  %s\n  (run `go test -tags libsqlite3 ./cmd/autosk -update` to refresh)",
			path,
			strings.TrimRight(string(want), "\n"),
			strings.TrimRight(string(got), "\n"))
	}
}
