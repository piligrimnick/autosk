package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"autosk/internal/daemon/api"
	"autosk/internal/daemon/client"
	"autosk/internal/daemon/pi"
)

// TestE2E_AttachClient_FullFlowAgainstFakepi exercises the entire
// `autosk attach` Go path (no Bubble Tea — that's covered by the
// in-package unit tests) end-to-end against a real daemon over UDS
// with a fakepi runner. It pins the contract the TUI relies on:
//
//  1. Stream(?attach=true) bumps the attach counter to 1.
//  2. SendInput on an idle runner is dispatched as "prompt" and
//     reaches fakepi's stdin.
//  3. A second concurrent Stream(?attach=true) lifts the counter
//     to 2 AND surfaces a fresh `status` SSE frame to the first
//     client whose attach_count == 2.
//  4. Detaching the second client (cancel its stream context)
//     drops the counter back to 1 and the first client sees the
//     transition as another `status` event.
//  5. Abort lands on fakepi and the daemon answers OK.
//  6. The first stream cleanly drains to channel-close when its
//     context is cancelled — no leaked goroutine on the producer
//     side.
//
// The test deliberately avoids the Bubble Tea program. Driving the
// program from a non-TTY would require a pseudo-terminal harness;
// the model+update tests already cover the reducer.
func TestE2E_AttachClient_FullFlowAgainstFakepi(t *testing.T) {
	bin := buildAttachFakepi(t)
	d := startAttachE2E(t)
	root, jobID := seedAttachRun(t)

	// Spawn a real *pi.Runner so the daemon has a registered handle.
	runner, err := pi.Spawn(context.Background(), pi.Opts{PIBin: bin})
	if err != nil {
		t.Fatalf("pi.Spawn: %v", err)
	}
	t.Cleanup(func() {
		_ = runner.CloseStdin()
		_, _ = runner.Wait(context.Background(), 2*time.Second)
	})
	go func() {
		for range runner.Events() {
		}
	}()
	d.runners.Register(jobID, runner)
	defer d.runners.Unregister(jobID)

	// Build a typed client over the same UDS the fixture set up.
	clientA, err := client.New(client.Options{
		Sock: d.sockPath,
		Cwd:  root,
		HTTPClient: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var dialer net.Dialer
					return dialer.DialContext(ctx, "unix", d.sockPath)
				},
			},
			Timeout: 0,
		},
	})
	if err != nil {
		t.Fatalf("client.New A: %v", err)
	}

	// (1) Open attached SSE stream A and expect attach_count == 1.
	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	handleA, err := clientA.Stream(ctxA, jobID, client.StreamOptions{Attach: true, Full: true})
	if err != nil {
		t.Fatalf("clientA.Stream: %v", err)
	}
	defer handleA.Close()

	waitForStatusAttach(t, handleA, 1, 2*time.Second)

	// (2) SendInput is dispatched as prompt on an idle runner.
	resp, err := clientA.SendInput(context.Background(), jobID, "hello fake", "")
	if err != nil {
		t.Fatalf("clientA.SendInput: %v", err)
	}
	if resp.Dispatched != "prompt" {
		t.Fatalf("Dispatched=%q want prompt", resp.Dispatched)
	}

	// (3) Open attached SSE stream B. clientA should see attach_count==2.
	clientB, err := client.New(client.Options{
		Sock: d.sockPath,
		Cwd:  root,
		HTTPClient: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var dialer net.Dialer
					return dialer.DialContext(ctx, "unix", d.sockPath)
				},
			},
			Timeout: 0,
		},
	})
	if err != nil {
		t.Fatalf("client.New B: %v", err)
	}
	ctxB, cancelB := context.WithCancel(context.Background())
	handleB, err := clientB.Stream(ctxB, jobID, client.StreamOptions{Attach: true})
	if err != nil {
		t.Fatalf("clientB.Stream: %v", err)
	}
	t.Cleanup(handleB.Close)
	waitForStatusAttach(t, handleA, 2, 2*time.Second)

	// (4) Detach B; A sees attach_count flip back to 1.
	cancelB()
	handleB.Close()
	waitForStatusAttach(t, handleA, 1, 2*time.Second)

	// (5) Abort: daemon answers OK.
	abortResp, err := clientA.Abort(context.Background(), jobID)
	if err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if !abortResp.OK {
		t.Fatalf("Abort OK=%v", abortResp.OK)
	}

	// (6) Cancel A; the stream channel must drain to closed within a beat.
	cancelA()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-handleA.Events:
			if !ok {
				goto drained
			}
		case <-deadline:
			t.Fatal("stream A never closed after context cancel")
		}
	}
drained:
	// Counter must have returned to 0.
	deadline2 := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline2) {
		if d.attachments.Count(jobID) == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := d.attachments.Count(jobID); got != 0 {
		t.Fatalf("attach count did not return to 0 after both detached: %d", got)
	}
}

// waitForStatusAttach drains events off h.Events until it sees a
// status frame with AttachCount==want, or the timeout fires. Other
// event kinds (message, done) are silently skipped so the assertion
// is robust against transcript chatter from the run.
func waitForStatusAttach(t *testing.T, h *client.StreamHandle, want int, dur time.Duration) {
	t.Helper()
	deadline := time.After(dur)
	for {
		select {
		case ev, ok := <-h.Events:
			if !ok {
				t.Fatalf("stream closed before status with attach_count=%d", want)
			}
			if ev.Type != client.EventTypeStatus || ev.Status == nil {
				continue
			}
			if ev.Status.AttachCount == want {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for status with attach_count=%d", want)
		}
	}
}

// TestE2E_AttachClient_UnknownJobIs404 documents the failure mode for
// the CLI's "attach to a typo'd job-id" case: client.Stream returns
// an APIError(404) before any goroutines are spawned, and the
// daemon's attach counter stays at zero.
func TestE2E_AttachClient_UnknownJobIs404(t *testing.T) {
	d := startAttachE2E(t)
	root := initFixtureProject(t)

	c, err := client.New(client.Options{
		Sock: d.sockPath,
		Cwd:  root,
		HTTPClient: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var dialer net.Dialer
					return dialer.DialContext(ctx, "unix", d.sockPath)
				},
			},
			Timeout: 5 * time.Second,
		},
	})
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}

	_, err = c.Stream(context.Background(), "no-such-job", client.StreamOptions{Attach: true})
	if err == nil {
		t.Fatal("expected error for unknown job")
	}
	ae, ok := client.IsAPIError(err)
	if !ok {
		t.Fatalf("err=%v want APIError", err)
	}
	if ae.Status != http.StatusNotFound {
		t.Fatalf("status=%d want 404", ae.Status)
	}
	// Counter must remain at zero — the SSE handler validates the
	// run before touching Attachments.Acquire.
	if got := d.attachments.Count("no-such-job"); got != 0 {
		t.Fatalf("attach counter for unknown job=%d (want 0)", got)
	}
}

// (silence unused-import lint if any of the imports here drift.)
var _ = filepath.Join
var _ = fmt.Sprint
var _ = api.MessageEvent{}
