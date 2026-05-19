package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"autosk/internal/daemon/api"
	"autosk/internal/daemon/runstore"
	"autosk/internal/daemon/transcript"
)

// handleStream implements GET /v1/jobs/{id}/stream as a Server-Sent Events
// endpoint. Sends three event kinds:
//
//	event: message  — one projected transcript event
//	event: status   — run status snapshot (whenever it changes)
//	event: done     — terminal status reached; the server closes the connection
//
// Query params:
//
//	?limit=N  — initial replay size (default 20, max 500)
//	?full=true — replay the entire transcript before tailing
//
// `Last-Event-ID` is honoured: when present and numeric, we skip that
// many leading events on first replay so reconnects don't duplicate.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	proj := projectFromCtx(r.Context())
	if proj == nil {
		writeError(w, http.StatusInternalServerError, "missing project context", nil)
		return
	}
	jobID := r.PathValue("job_id")
	run, err := proj.Runs.GetRun(r.Context(), jobID)
	if err != nil {
		if errors.Is(err, runstore.ErrNotFound) {
			writeError(w, http.StatusNotFound, "job not found", nil)
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error(), nil)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported", nil)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	q := r.URL.Query()
	full := q.Get("full") == "true"
	limit := 20
	if l := q.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err == nil && n >= 0 {
			if n > 500 {
				n = 500
			}
			limit = n
		}
	}

	cursor := 0
	if lid := r.Header.Get("Last-Event-ID"); lid != "" {
		if n, err := strconv.Atoi(lid); err == nil && n > 0 {
			cursor = n
		}
	}

	// Initial replay.
	replayed, sent, err := replayInitial(w, flusher, run.SessionPath, full, limit, cursor)
	if err != nil {
		// We've already started writing; surface as an SSE event to be
		// kind to the client.
		writeSSEEvent(w, flusher, "error", 0, map[string]string{"error": err.Error()})
		return
	}

	// Status snapshot.
	writeSSEEvent(w, flusher, "status", sent, api.FromRun(run))
	if run.Status.IsTerminal() {
		writeSSEEvent(w, flusher, "done", sent, api.FromRun(run))
		return
	}
	cursor = replayed

	// Tail loop: poll runstore + transcript until terminal or client disconnects.
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	prevStatus := run.Status
	prevSession := run.SessionPath
	prevCorrections := run.CorrectionsUsed
	prevPID := pidOrZero(run.PID)
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			cur, err := proj.Runs.GetRun(r.Context(), jobID)
			if err != nil {
				return
			}
			// If session_path became available, switch to tailing it.
			if prevSession == "" && cur.SessionPath != "" {
				prevSession = cur.SessionPath
			}
			// Pull any new transcript events.
			if cur.SessionPath != "" {
				newCursor, n, err := pumpTranscript(w, flusher, cur.SessionPath, cursor, &sent)
				if err == nil {
					cursor = newCursor
					_ = n
				}
			}
			// Status / interesting field changes.
			if cur.Status != prevStatus ||
				cur.CorrectionsUsed != prevCorrections ||
				pidOrZero(cur.PID) != prevPID {
				writeSSEEvent(w, flusher, "status", sent, api.FromRun(cur))
				prevStatus = cur.Status
				prevCorrections = cur.CorrectionsUsed
				prevPID = pidOrZero(cur.PID)
			}
			if cur.Status.IsTerminal() {
				writeSSEEvent(w, flusher, "done", sent, api.FromRun(cur))
				return
			}
		}
	}
}

// replayInitial reads the transcript and writes "message" events for the
// first chunk. Returns (eventsParsed, sentTotalSoFar, err).
//
// When session_path is empty (run hasn't started a session yet), this is
// a no-op so the caller can proceed to the polling loop.
func replayInitial(w http.ResponseWriter, fl http.Flusher, path string, full bool, limit, skip int) (int, int, error) {
	if path == "" {
		return 0, 0, nil
	}
	events, err := transcript.Read(path)
	if err != nil {
		if errors.Is(err, transcript.ErrMissing) {
			return 0, 0, nil
		}
		return 0, 0, err
	}
	all := events
	start := 0
	if skip > 0 && skip < len(all) {
		start = skip
	} else if !full && limit > 0 && limit < len(all)-start {
		start = len(all) - limit
	}
	sent := 0
	for i := start; i < len(all); i++ {
		writeSSEEvent(w, fl, "message", i+1, toAPIEvent(all[i]))
		sent++
	}
	return len(all), sent, nil
}

// pumpTranscript reads the transcript, emits any events whose index is
// >= cursor, and returns the new cursor + count emitted.
func pumpTranscript(w http.ResponseWriter, fl http.Flusher, path string, cursor int, sentCounter *int) (int, int, error) {
	events, err := transcript.Read(path)
	if err != nil {
		return cursor, 0, err
	}
	if cursor >= len(events) {
		return cursor, 0, nil
	}
	n := 0
	for i := cursor; i < len(events); i++ {
		*sentCounter++
		writeSSEEvent(w, fl, "message", i+1, toAPIEvent(events[i]))
		n++
	}
	return len(events), n, nil
}

// writeSSEEvent writes one SSE message and flushes the connection.
func writeSSEEvent(w http.ResponseWriter, fl http.Flusher, name string, id int, payload any) {
	buf, err := json.Marshal(payload)
	if err != nil {
		return
	}
	if name != "" {
		fmt.Fprintf(w, "event: %s\n", name)
	}
	if id > 0 {
		fmt.Fprintf(w, "id: %d\n", id)
	}
	fmt.Fprintf(w, "data: %s\n\n", buf)
	fl.Flush()
}

func toAPIEvent(e transcript.Event) api.MessageEvent {
	return api.MessageEvent{
		Kind:    string(e.Kind),
		TS:      e.TS,
		Text:    e.Text,
		Name:    e.Name,
		Input:   rawOrNil(e.Input),
		IsError: e.IsError,
		Raw:     rawOrNil(e.Raw),
	}
}

func pidOrZero(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}
