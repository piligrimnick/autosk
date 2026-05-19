package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"autosk/internal/daemon/api"
	"autosk/internal/daemon/pi"
	"autosk/internal/daemon/pirunners"
	"autosk/internal/daemon/runstore"
)

// handleInput implements POST /v1/jobs/{id}/input.
//
// The daemon dispatches the operator's text into pi as:
//
//   - "prompt" when the pi runner is idle (no in-flight agent_start),
//   - "steer"  when pi is streaming and the client did not opt into
//     "follow_up" (default behaviour),
//   - "follow_up" when the client passed StreamingBehavior:"follow_up".
//
// Concurrent writers are not rejected: pi RPC serialises them on its
// own (a `prompt` is a synchronous preflight ack, `steer`/`follow_up`
// queue between turns). Every accepted input also shows up on the SSE
// stream of every other reader as a regular user_text message.
//
// Per docs/plans/20260519-Attach-Plan.md §4.3.
func (s *Server) handleInput(w http.ResponseWriter, r *http.Request) {
	proj := projectFromCtx(r.Context())
	if proj == nil {
		writeError(w, http.StatusInternalServerError, "missing project context", nil)
		return
	}
	if s.deps.Runners == nil {
		writeError(w, http.StatusServiceUnavailable, "attach disabled: no runner registry", nil)
		return
	}
	jobID := r.PathValue("job_id")

	// Run must exist (404) and be non-terminal (409). A terminal run
	// could otherwise leak a 500 from the registry lookup below.
	run, err := proj.Runs.GetRun(r.Context(), jobID)
	if err != nil {
		if errors.Is(err, runstore.ErrNotFound) {
			writeError(w, http.StatusNotFound, "job not found", nil)
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error(), nil)
		return
	}
	if run.Status.IsTerminal() {
		writeError(w, http.StatusConflict, "run is terminal", map[string]any{
			"status": string(run.Status),
		})
		return
	}

	var req api.InputRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "decode body: "+err.Error(), nil)
		return
	}
	if req.Message == "" {
		writeError(w, http.StatusBadRequest, "message is required", nil)
		return
	}
	switch req.StreamingBehavior {
	case "", "steer", "follow_up":
		// ok
	default:
		writeError(w, http.StatusBadRequest, "streamingBehavior must be 'steer' or 'follow_up'", nil)
		return
	}

	handle, err := s.deps.Runners.Get(jobID)
	if err != nil {
		if errors.Is(err, pirunners.ErrNotRegistered) {
			// No live runner for this job. Common cases: run is queued
			// but the worker hasn't spawned pi yet, the run is terminal
			// but the runstore status hasn't been written back yet, or
			// the agent is a custom JS runner that the registry doesn't
			// track. 409 conveys "service exists but not ready".
			writeError(w, http.StatusConflict, "runner not registered for job", map[string]any{
				"job_id": jobID,
			})
			return
		}
		writeError(w, http.StatusInternalServerError, "registry: "+err.Error(), nil)
		return
	}

	dispatched, dispErr := dispatchInput(r.Context(), handle, req)
	if dispErr != nil {
		dispErr.write(w, dispatched)
		return
	}

	writeJSON(w, http.StatusOK, api.InputResponse{JobID: jobID, Dispatched: dispatched})
}

// dispatchErr carries the structured failure detail for a rejected
// input dispatch. It's used so handleInput can return a single 422
// response that includes the dispatched command AND pi's live streaming
// state — important context for clients that race the agent_end frame.
type dispatchErr struct {
	status  int
	message string
	details map[string]any
}

func (e *dispatchErr) write(w http.ResponseWriter, dispatched string) {
	details := e.details
	if details == nil {
		details = map[string]any{}
	}
	if dispatched != "" {
		details["dispatched"] = dispatched
	}
	if len(details) == 0 {
		details = nil
	}
	writeError(w, e.status, e.message, details)
}

// dispatchInput sends the operator's input to pi, retrying once on a
// dispatch-shape rejection that looks like a TOCTOU between
// IsStreaming() and SendCommand: the agent_end frame can land between
// the two atomic reads, so we'd send "steer" while pi is already idle
// (or "prompt" while pi flipped to streaming), and pi answers
// success=false with a state-mismatch error.
//
// Pi-level rejections that are NOT state-mismatch (message too long,
// content rejected, abort already in flight, …) intentionally don't
// trigger a retry — the same payload sent under the opposite shape
// would just fail the same way and risk pi accepting a steer the
// operator didn't ask for. We bail out with a 422 + the first_error so
// the caller can decide whether to retry by hand.
//
// Returns the final "dispatched" label so the response body reflects what
// pi actually accepted. On error returns a *dispatchErr with the live
// IsStreaming() snapshot so clients can debug the race.
func dispatchInput(ctx context.Context, handle pirunners.RunnerHandle, req api.InputRequest) (string, *dispatchErr) {
	streaming := handle.IsStreaming()
	cmd, dispatched := buildInputCommand(req, streaming)
	resp, derr := sendAndWait(ctx, handle, cmd)
	if derr != nil {
		return dispatched, derr
	}
	if resp.Success {
		return dispatched, nil
	}
	if !isStateMismatchError(resp.Error) {
		return dispatched, &dispatchErr{
			status:  http.StatusUnprocessableEntity,
			message: fmt.Sprintf("pi rejected %s: %s", dispatched, resp.Error),
			details: map[string]any{
				"streaming":    streaming,
				"pi_error":     resp.Error,
				"retry":        false,
				"retry_reason": "non_state_mismatch",
			},
		}
	}
	// State-mismatch error — retry once with the opposite dispatch shape.
	// Safe under the locked v1 serialisation rules: a concurrent retry
	// just races other writers on pi's stdin lock exactly like the
	// initial send.
	retryCmd, retryDispatched := buildInputCommand(req, !streaming)
	retryResp, derr := sendAndWait(ctx, handle, retryCmd)
	if derr != nil {
		return retryDispatched, derr
	}
	if retryResp.Success {
		return retryDispatched, nil
	}
	return retryDispatched, &dispatchErr{
		status:  http.StatusUnprocessableEntity,
		message: fmt.Sprintf("pi rejected %s after retry from %s: %s", retryDispatched, dispatched, retryResp.Error),
		details: map[string]any{
			"streaming":     handle.IsStreaming(),
			"first_attempt": dispatched,
			"first_error":   resp.Error,
			"retry_attempt": retryDispatched,
			"retry_error":   retryResp.Error,
		},
	}
}

// isStateMismatchError returns true if pi's success=false error text
// looks like a streaming-state mismatch — i.e. pi rejected the command
// because the runner flipped between IsStreaming() and SendCommand.
// Conservative on purpose: an unrecognised error pattern falls through
// to a one-shot 422 (no wasted retry, no surprise-accept on the wrong
// shape). The token list mirrors pi 0.74's error messages for the
// prompt/steer/follow_up commands and is kept small so we don't catch
// real pi-level rejections ("message too long", "abort in flight", …).
//
// If pi ever grows a structured `reason:"state_mismatch"` field on its
// response, this helper can be replaced with a single field check.
func isStateMismatchError(s string) bool {
	if s == "" {
		return false
	}
	lower := strings.ToLower(s)
	tokens := []string{
		"not streaming",
		"already streaming",
		"no run",
		"no active run",
		"no_active_run",
		"idle",
		"in_progress",
		"not in_progress",
		"state mismatch",
		"state_mismatch",
	}
	for _, t := range tokens {
		if strings.Contains(lower, t) {
			return true
		}
	}
	return false
}

func sendAndWait(ctx context.Context, handle pirunners.RunnerHandle, cmd pi.Command) (pi.Response, *dispatchErr) {
	ch, err := handle.SendCommand(cmd)
	if err != nil {
		return pi.Response{}, &dispatchErr{
			status:  http.StatusInternalServerError,
			message: "send command: " + err.Error(),
		}
	}
	select {
	case resp, ok := <-ch:
		if !ok {
			return pi.Response{}, &dispatchErr{
				status:  http.StatusServiceUnavailable,
				message: "runner closed before reply",
			}
		}
		return resp, nil
	case <-ctx.Done():
		return pi.Response{}, &dispatchErr{
			status:  http.StatusGatewayTimeout,
			message: "client disconnected before pi acked",
		}
	}
}

// buildInputCommand picks the pi command shape (prompt / steer / follow_up)
// given the operator's input and pi's current state. Exposed for tests.
func buildInputCommand(req api.InputRequest, streaming bool) (pi.Command, string) {
	if !streaming {
		// Idle: a plain prompt. Don't pass streamingBehavior — when
		// pi is idle it's a no-op, but omitting it keeps the wire
		// shape identical to the daemon's own initial prompt.
		return pi.Command{Type: "prompt", Message: req.Message}, "prompt"
	}
	switch req.StreamingBehavior {
	case "follow_up":
		return pi.Command{Type: "follow_up", Message: req.Message}, "follow_up"
	default:
		return pi.Command{Type: "steer", Message: req.Message}, "steer"
	}
}

// handleAbort implements POST /v1/jobs/{id}/abort.
//
// Asks pi to stop the in-flight agent run. The run row stays
// non-terminal (DELETE /v1/jobs/{id} is the way to cancel the whole
// job); the abort just unblocks the turn loop so the operator can
// regain control of the pi prompt.
func (s *Server) handleAbort(w http.ResponseWriter, r *http.Request) {
	proj := projectFromCtx(r.Context())
	if proj == nil {
		writeError(w, http.StatusInternalServerError, "missing project context", nil)
		return
	}
	if s.deps.Runners == nil {
		writeError(w, http.StatusServiceUnavailable, "attach disabled: no runner registry", nil)
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
	if run.Status.IsTerminal() {
		writeError(w, http.StatusConflict, "run is terminal", map[string]any{
			"status": string(run.Status),
		})
		return
	}

	handle, err := s.deps.Runners.Get(jobID)
	if err != nil {
		if errors.Is(err, pirunners.ErrNotRegistered) {
			writeError(w, http.StatusConflict, "runner not registered for job", map[string]any{
				"job_id": jobID,
			})
			return
		}
		writeError(w, http.StatusInternalServerError, "registry: "+err.Error(), nil)
		return
	}
	if err := handle.Abort(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "abort: "+err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, api.AbortResponse{JobID: jobID, OK: true})
}
