package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"autosk/internal/daemon/api"
	"autosk/internal/daemon/runstore"
)

// buildAutoskBin compiles a fresh ./bin/autosk for this test.
var (
	autoskOnce sync.Once
	autoskBin  string
	autoskErr  error
)

func buildAutosk(t *testing.T) string {
	t.Helper()
	autoskOnce.Do(func() {
		dir, err := os.MkdirTemp("", "autosk-e2e-")
		if err != nil {
			autoskErr = err
			return
		}
		bin := filepath.Join(dir, "autosk")
		cmd := exec.Command("go", "build", "-tags", "libsqlite3", "-o", bin, ".")
		cmd.Env = os.Environ()
		out, err := cmd.CombinedOutput()
		if err != nil {
			autoskErr = err
			t.Logf("go build autosk: %s", out)
			return
		}
		autoskBin = bin
	})
	if autoskErr != nil {
		t.Skipf("build autosk: %v", autoskErr)
	}
	return autoskBin
}

// buildFakepiForE2E compiles internal/daemon/pi/fakepi/ to a tmp file.
var (
	fakepiOnce sync.Once
	fakepiBin  string
	fakepiErr  error
)

func buildFakepi(t *testing.T) string {
	t.Helper()
	fakepiOnce.Do(func() {
		dir, err := os.MkdirTemp("", "autosk-fakepi-e2e-")
		if err != nil {
			fakepiErr = err
			return
		}
		bin := filepath.Join(dir, "fakepi")
		cmd := exec.Command("go", "build", "-o", bin, "../../internal/daemon/pi/fakepi")
		cmd.Env = os.Environ()
		out, err := cmd.CombinedOutput()
		if err != nil {
			fakepiErr = err
			t.Logf("go build fakepi: %s", out)
			return
		}
		fakepiBin = bin
	})
	if fakepiErr != nil {
		t.Skipf("build fakepi: %v", fakepiErr)
	}
	return fakepiBin
}

// pickPort grabs a free TCP port to avoid conflicts in CI.
func pickPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// E2E: spawn autosk daemon with fakepi, POST a job, observe status=done.
func TestE2E_DaemonSpawnsFakePiAndCompletes(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e is long")
	}
	binAutosk := buildAutosk(t)
	binFake := buildFakepi(t)

	dir := t.TempDir()
	projectDir := filepath.Join(dir, "proj")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Seed .autosk/db by running `autosk init` in projectDir.
	initCmd := exec.Command(binAutosk, "init")
	initCmd.Dir = projectDir
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Fatalf("init: %v: %s", err, out)
	}

	addr := pickPort(t)
	sessionRoot := filepath.Join(projectDir, ".autosk", "sessions")
	// Pre-create the session file fakepi will reference.
	sessFile := filepath.Join(dir, "fake-session.jsonl")
	if err := os.WriteFile(sessFile, []byte(
		`{"type":"session","id":"sess-fake","timestamp":"2026-05-17T10:00:00Z","cwd":"/x"}`+"\n"+
			`{"type":"message","id":"e1","parentId":null,"timestamp":"2026-05-17T10:00:01Z","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}`+"\n",
	), 0o644); err != nil {
		t.Fatal(err)
	}

	// Boot the daemon.
	srvCtx, cancelSrv := context.WithCancel(context.Background())
	defer cancelSrv()
	srvCmd := exec.CommandContext(srvCtx, binAutosk, "daemon", "serve",
		"--bind", addr, "--workers", "1",
		"--pi-bin", binFake,
		"--cwd", projectDir,
		"--session-dir", sessionRoot,
		"--grace", "2s",
		"--idle-timeout", "5s",
	)
	srvCmd.Env = append(os.Environ(),
		"FAKEPI_SESSION_ID=sess-fake",
		"FAKEPI_SESSION_FILE="+sessFile,
	)
	var srvStderr bytes.Buffer
	srvCmd.Stderr = &srvStderr
	if err := srvCmd.Start(); err != nil {
		t.Fatalf("daemon start: %v", err)
	}
	defer func() {
		_ = srvCmd.Process.Signal(os.Interrupt)
		_, _ = srvCmd.Process.Wait()
		t.Logf("daemon stderr:\n%s", srvStderr.String())
	}()

	// Wait until /healthz responds.
	if err := waitForHealth("http://"+addr, 5*time.Second); err != nil {
		t.Fatalf("daemon never came up: %v\nstderr=%s", err, srvStderr.String())
	}

	// POST /v1/jobs.
	body, _ := json.Marshal(api.SubmitRequest{Prompt: "hello"})
	resp, err := http.Post("http://"+addr+"/v1/jobs", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("submit status %d body=%s", resp.StatusCode, b)
	}
	var job api.JobResponse
	if err := json.NewDecoder(resp.Body).Decode(&job); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// Poll for terminal.
	deadline := time.Now().Add(15 * time.Second)
	var finalJob api.JobResponse
	for time.Now().Before(deadline) {
		r, err := http.Get("http://" + addr + "/v1/jobs/" + job.JobID)
		if err != nil {
			t.Fatal(err)
		}
		_ = json.NewDecoder(r.Body).Decode(&finalJob)
		r.Body.Close()
		if runstore.RunStatus(finalJob.Status).IsTerminal() {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if finalJob.Status != string(runstore.StatusDone) {
		t.Fatalf("not done: %+v\ndaemon stderr:\n%s", finalJob, srvStderr.String())
	}
	if finalJob.PISessionID != "sess-fake" {
		t.Errorf("pi_session_id: %q", finalJob.PISessionID)
	}

	// And messages should be servable.
	r, _ := http.Get("http://" + addr + "/v1/jobs/" + job.JobID + "/messages?limit=5")
	if r.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(r.Body)
		t.Fatalf("messages status %d body=%s", r.StatusCode, b)
	}
	var msgs api.MessagesResponse
	_ = json.NewDecoder(r.Body).Decode(&msgs)
	r.Body.Close()
	if len(msgs.Events) == 0 {
		t.Fatalf("expected events, got 0")
	}
	if !containsKind(msgs.Events, "assistant_text") {
		t.Errorf("missing assistant_text event in %+v", msgs.Events)
	}
}

func waitForHealth(base string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		r, err := http.Get(base + "/v1/healthz")
		if err == nil && r.StatusCode == 200 {
			r.Body.Close()
			return nil
		}
		if r != nil {
			r.Body.Close()
		}
		time.Sleep(50 * time.Millisecond)
	}
	return errors.New("timeout")
}

func containsKind(events []api.MessageEvent, kind string) bool {
	for _, e := range events {
		if e.Kind == kind {
			return true
		}
	}
	return false
}

// Sanity: avoid an import-unused warning when stripping bits during dev.
var _ = strings.TrimSpace
