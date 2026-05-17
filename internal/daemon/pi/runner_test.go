package pi_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"autosk/internal/daemon/pi"
)

// fakepiBin is built once for the test binary lifetime.
var (
	fakepiOnce sync.Once
	fakepiPath string
	fakepiErr  error
)

func buildFakepi(t *testing.T) string {
	t.Helper()
	fakepiOnce.Do(func() {
		dir, err := os.MkdirTemp("", "autosk-fakepi-")
		if err != nil {
			fakepiErr = err
			return
		}
		bin := filepath.Join(dir, "fakepi")
		// Build the fakepi binary so we can exec it in tests.
		cmd := exec.Command("go", "build", "-o", bin, "./fakepi")
		// Run inside this package's directory.
		cmd.Dir = "."
		out, err := cmd.CombinedOutput()
		if err != nil {
			fakepiErr = err
			t.Logf("go build fakepi: %s", out)
			return
		}
		fakepiPath = bin
	})
	if fakepiErr != nil {
		t.Skipf("cannot build fakepi: %v", fakepiErr)
	}
	return fakepiPath
}

func newRunner(t *testing.T, extraEnv ...string) *pi.Runner {
	t.Helper()
	bin := buildFakepi(t)
	env := append(os.Environ(), extraEnv...)
	r, err := pi.Spawn(context.Background(), pi.Opts{
		PIBin: bin,
		Env:   env,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() {
		_ = r.CloseStdin()
		_, _ = r.Wait(context.Background(), 2*time.Second)
	})
	return r
}

func TestRunner_GetState(t *testing.T) {
	r := newRunner(t, "FAKEPI_SESSION_ID=abc", "FAKEPI_SESSION_FILE=/tmp/x.jsonl")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	info, err := r.GetState(ctx)
	if err != nil {
		t.Fatalf("GetState: %v", err)
	}
	if info.SessionID != "abc" || info.SessionFile != "/tmp/x.jsonl" {
		t.Fatalf("session info: %+v", info)
	}
}

func TestRunner_PromptThenAgentEnd(t *testing.T) {
	r := newRunner(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := r.SendPrompt(ctx, "hello"); err != nil {
		t.Fatalf("SendPrompt: %v", err)
	}
	if err := r.WaitForAgentEnd(ctx); err != nil {
		t.Fatalf("WaitForAgentEnd: %v", err)
	}

	// Drain events to confirm we saw the expected kinds.
	timeout := time.After(500 * time.Millisecond)
	seen := map[pi.EventKind]bool{}
LOOP:
	for {
		select {
		case e, ok := <-r.Events():
			if !ok {
				break LOOP
			}
			seen[e.Kind] = true
			if seen[pi.KindAgentEnd] {
				// Once agent_end is seen we can stop reading.
				break LOOP
			}
		case <-timeout:
			break LOOP
		}
	}
	for _, want := range []pi.EventKind{
		pi.KindResponse, pi.KindAgentStart, pi.KindTurnStart,
		pi.KindMessageStart, pi.KindMessageEnd, pi.KindTurnEnd,
		pi.KindAgentEnd,
	} {
		if !seen[want] {
			t.Errorf("did not observe %q event", want)
		}
	}
}

func TestRunner_PromptError(t *testing.T) {
	r := newRunner(t, "FAKEPI_SCENARIO=prompt_error")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := r.SendPrompt(ctx, "x"); err == nil {
		t.Fatal("expected prompt error, got nil")
	}
}

func TestRunner_DialogIsAutoCancelled(t *testing.T) {
	// fakepi emits a `select` dialog mid-run; the runner should reply
	// {cancelled:true} and pi should still finish (we'd see agent_end).
	r := newRunner(t, "FAKEPI_SCENARIO=dialog")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := r.SendPrompt(ctx, "x"); err != nil {
		t.Fatalf("SendPrompt: %v", err)
	}
	if err := r.WaitForAgentEnd(ctx); err != nil {
		t.Fatalf("WaitForAgentEnd: %v", err)
	}
}

func TestRunner_CleanShutdownOnStdinClose(t *testing.T) {
	r := newRunner(t)
	if err := r.CloseStdin(); err != nil {
		t.Fatalf("CloseStdin: %v", err)
	}
	code, err := r.Wait(context.Background(), 3*time.Second)
	if pi.IsWaitTimeout(err) {
		t.Fatalf("child did not exit on stdin close")
	}
	if code != 0 {
		t.Errorf("exit code: %d", code)
	}
}

func TestRunner_TerminateThenWait(t *testing.T) {
	// no_agent_end keeps pi alive: SIGTERM should bring it down.
	r := newRunner(t, "FAKEPI_SCENARIO=no_agent_end")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := r.SendPrompt(ctx, "x"); err != nil {
		t.Fatalf("SendPrompt: %v", err)
	}
	// Make sure we never get agent_end here.
	wctx, wcancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer wcancel()
	if err := r.WaitForAgentEnd(wctx); err == nil {
		t.Fatal("expected no agent_end")
	}
	if err := r.Terminate(); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	_, err := r.Wait(context.Background(), 3*time.Second)
	if pi.IsWaitTimeout(err) {
		_ = r.Kill()
		t.Fatal("child did not exit after SIGTERM")
	}
}
