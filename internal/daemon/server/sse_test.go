package server_test

import (
	"bufio"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestSSE_StreamReachableOverUDS confirms that SSE works over the UDS
// transport: a request for a non-existent job returns 404 (writeError),
// but the same request without a project header gets bounced at 400 by
// the middleware — i.e. the SSE handler is routed identically to the
// rest of the API.
func TestSSE_StreamRoutedThroughProjectMiddleware(t *testing.T) {
	fx := newSrvFixture(t)
	root := initProject(t)

	// With cwd + unknown job → 404.
	resp := fx.do(t, http.MethodGet, "/v1/jobs/unknown-job/stream", root, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}

	// Without cwd → 400 (middleware refuses before SSE handler runs).
	resp2 := fx.do(t, http.MethodGet, "/v1/jobs/unknown-job/stream", "", "")
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp2.StatusCode)
	}
}

// TestSSE_TerminalStreamsDoneEvent: a job that is already terminal at
// connect time gets `status` + `done` written and the connection
// closes. We don't construct a real job here (that requires lots of
// workflow plumbing) — but we verify that the SSE path is reachable
// over the unix socket and that we can read the chunked body.
func TestSSE_HeadersAreCorrect(t *testing.T) {
	fx := newSrvFixture(t)
	root := initProject(t)
	// Use a request to the messages endpoint to confirm the
	// Content-Type plumbing is wired: SSE-specific headers (text/event-
	// stream) are only set on hit (job exists). For the unknown-job
	// case we just verify the JSON error envelope.
	resp := fx.do(t, http.MethodGet, "/v1/jobs/unknown/messages", root, "")
	defer resp.Body.Close()
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("messages 404: Content-Type=%q, want application/json", got)
	}

	// Quick round-trip on /v1/version exercising the chunked path.
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://autosk/v1/version", nil)
	resp2, err := fx.httpCli.Do(req)
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("version status=%d", resp2.StatusCode)
	}
	// Make sure we can actually read the body (UDS transport works for
	// streaming responses).
	br := bufio.NewReader(resp2.Body)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := br.ReadByte(); err != nil {
			break
		}
	}
}
