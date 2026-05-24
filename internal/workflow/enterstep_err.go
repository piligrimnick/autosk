package workflow

import (
	"errors"
	"fmt"
)

// EnterStepHint is a friendly wrapper around an EnterStep error. It
// decouples the human-readable message from the underlying typed
// error so callers (CLI, lazy TUI) can render a clean one-liner while
// errors.As still finds the typed reason via Unwrap.
//
// Use MapEnterStepError to construct one; the zero value is not
// useful.
type EnterStepHint struct {
	// Msg is the rendered, copy-pasteable user message — already
	// formatted with the taskID and step name. It must not embed the
	// wrapped error's own Error() text (that's what Unwrap is for):
	// duplicating "step_max_visits_exceeded: step …" inside the hint
	// makes the lazy flash bar unreadable.
	Msg string
	// Wrapped is the underlying typed error (e.g.
	// MaxVisitsExceededError). Surfaced via Unwrap so callers can
	// errors.As through the friendly message.
	Wrapped error
}

func (e *EnterStepHint) Error() string { return e.Msg }
func (e *EnterStepHint) Unwrap() error { return e.Wrapped }

// MapEnterStepError translates a workflow.EnterStep error into a
// user-facing message. The hint is identical across every surface
// that calls EnterStep — CLI verbs (`autosk enroll/resume/create`)
// and the lazy TUI — so an operator who bounces between the two sees
// the same copy-pasteable command line either way.
//
// For MaxVisitsExceededError we render:
//
//	cannot enter step "X": already at max_visits=N; reset with
//	`autosk metadata reset-visits <task-id> --step X` or resume to a
//	different step
//
// taskID is the task whose entry was rejected; it must be a concrete
// id (not a placeholder), so the printed command will run as-is.
// Other errors pass through verbatim (nil-in / nil-out).
func MapEnterStepError(taskID string, err error) error {
	if err == nil {
		return nil
	}
	var mve MaxVisitsExceededError
	if errors.As(err, &mve) {
		return &EnterStepHint{
			Msg: fmt.Sprintf(
				"cannot enter step %q: already at max_visits=%d; reset with `autosk metadata reset-visits %s --step %s` or resume to a different step",
				mve.StepName, mve.Max, taskID, mve.StepName,
			),
			Wrapped: mve,
		}
	}
	return err
}
