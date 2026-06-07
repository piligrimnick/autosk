package rpcclient

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestAutoSpawn_* exercise acceptance criterion #4 (with no daemon running, a
// read call transparently spawns autoskd and succeeds; concurrent first-callers
// do not double-spawn). They require the real autoskd binary; in plain Go CI
// (no Rust toolchain) they skip.

func findAutoskd(t *testing.T) string {
	t.Helper()
	if b := os.Getenv("AUTOSKD_BIN"); b != "" {
		return b
	}
	root := repoRoot(t)
	cand := filepath.Join(root, "target", "debug", "autoskd")
	if fi, err := os.Stat(cand); err == nil && !fi.IsDir() {
		return cand
	}
	t.Skip("autoskd binary not found (build `cargo build -p autoskd` or set AUTOSKD_BIN)")
	return ""
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("repo root (go.mod) not found")
		}
		dir = parent
	}
}

// seedProject creates a fresh v12 project via `autoskd init` and returns its
// root, isolating the daemon's registry under a temp HOME + a temp socket. It
// registers best-effort teardown that kills the auto-spawned daemon.
func seedProject(t *testing.T, bin string) (proj, sock string) {
	t.Helper()
	tmp := t.TempDir()
	proj = filepath.Join(tmp, "proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatalf("mkdir proj: %v", err)
	}
	home := filepath.Join(tmp, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("mkdir home: %v", err)
	}
	t.Setenv("HOME", home)
	// The socket must live on a SHORT path: AF_UNIX sun_path caps at ~104 bytes
	// and t.TempDir() embeds the (long) test name. Use a short temp dir.
	sockDir, err := os.MkdirTemp("", "as")
	if err != nil {
		t.Fatalf("mkdtemp sock: %v", err)
	}
	sock = filepath.Join(sockDir, "d.sock")
	t.Setenv("AUTOSK_SOCK", sock)
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	t.Cleanup(func() {
		// Best-effort: the connector spawns the daemon detached, so kill it by
		// its unique --sock cmdline.
		_ = exec.Command("pkill", "-f", "autoskd serve --sock "+sock).Run()
	})

	out, err := exec.Command(bin, "init", proj).CombinedOutput()
	if err != nil {
		t.Fatalf("autoskd init: %v\n%s", err, out)
	}
	return proj, sock
}

func TestAutoSpawn_ReadSucceeds(t *testing.T) {
	bin := findAutoskd(t)
	proj, sock := seedProject(t, bin)

	cli, err := New(Options{Sock: sock, Cwd: proj, AutoskdBin: bin})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// No daemon is running yet — this call must spawn it and succeed.
	agents, err := cli.Agents(ctx)
	if err != nil {
		t.Fatalf("agents (auto-spawn): %v", err)
	}
	var hasHuman bool
	for _, a := range agents {
		if a.Name == "human" && a.IsHuman {
			hasHuman = true
		}
	}
	if !hasHuman {
		t.Errorf("expected seeded human agent, got %+v", agents)
	}

	// Liveness probe also answers once the daemon is up.
	h, err := cli.Healthz(ctx)
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	if !h.OK {
		t.Errorf("healthz ok=false")
	}
}

func TestAutoSpawn_ConcurrentFirstCallersDoNotDeadlock(t *testing.T) {
	bin := findAutoskd(t)
	proj, sock := seedProject(t, bin)

	// Two clients with independent connectors race to spawn. autoskd's
	// single-instance binding means the loser exits 0 and both clients connect
	// to the winner; both calls must succeed.
	const n = 4
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			cli, err := New(Options{Sock: sock, Cwd: proj, AutoskdBin: bin})
			if err != nil {
				errs[idx] = err
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			_, errs[idx] = cli.Agents(ctx)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent caller %d failed: %v", i, err)
		}
	}
}
