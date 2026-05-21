package workflow

import (
	"context"
	"errors"
	"fmt"

	"autosk/internal/meta"
	"autosk/internal/store"
)

// MaxVisitsExceededError is returned by EnterStep when the next visit
// would cross the step's max_visits cap. The CLI / executor format this
// value to produce a useful daemon_runs.error string and a clean
// human-facing message.
//
// Visits is the count BEFORE the would-be increment (i.e. how many
// times the task has already entered the step), so a typical message
// reads "reached visits=N max=N" when the cap has been hit.
type MaxVisitsExceededError struct {
	StepID   string
	StepName string
	Visits   int
	Max      int
}

func (e MaxVisitsExceededError) Error() string {
	return fmt.Sprintf(
		"step_max_visits_exceeded: step %q reached visits=%d max=%d",
		e.StepName, e.Visits, e.Max,
	)
}

// EnterStepInput is the argument bag for EnterStep. WorkflowID is
// optional: when set (the create / enroll paths), it is stamped onto
// the task in the same transaction so the workflow_id, current_step_id,
// status, and step_visits counter all land atomically.
type EnterStepInput struct {
	TaskID     string
	StepID     string
	WorkflowID string // optional; when non-empty also stamped onto tasks.workflow_id
}

// taskStore is the subset of store.Store the helper depends on.
// store.Store satisfies it; pulled out so tests can stub a thin fake
// without dragging in raw SQL. We deliberately only require the atomic
// helper here — the helper owns the entire metadata + patch write, so
// no separate UpdateTask / UpdateMetadata callouts happen inside
// EnterStep.
type taskStore interface {
	UpdateMetadataAndPatch(ctx context.Context, id string, fn func(m map[string]any) error, p store.TaskPatch) (store.Task, error)
}

// EnterStep is the atomic "move a task into a workflow step" operation.
// Every code path that sets a task's current_step_id to a workflow step
// uses this so the visit counter and the task pointer stay in sync.
//
// Behaviour:
//
//  1. Look up the step row (we need its max_visits, name, and id).
//  2. Inside a single store transaction, read the task's
//     step_visits[step.ID] = visits.
//  3. If step.MaxVisits > 0 && visits + 1 > step.MaxVisits, return
//     MaxVisitsExceededError; the transaction rolls back, so neither
//     the counter nor the task pointer move.
//  4. Otherwise bump the counter to visits+1 and write the metadata +
//     patch (current_step_id = step.ID, status = 'work', and
//     when WorkflowID was set on the input also workflow_id) in one
//     UPDATE. Counter bump and pointer move land together — no
//     partial-success window between two transactions.
//
// Concurrency: doltlite is single-writer; the store helper serialises
// concurrent callers behind its write lane. Concurrent calls against
// the same task either both succeed in sequence (each observes the
// post-bump counter) or one is rejected when the cap fires.
func EnterStep(ctx context.Context, tasks taskStore, wfs *Store, in EnterStepInput) error {
	if in.TaskID == "" {
		return errors.New("EnterStep: TaskID required")
	}
	if in.StepID == "" {
		return errors.New("EnterStep: StepID required")
	}
	if wfs == nil {
		return errors.New("EnterStep: workflow store is nil")
	}

	stepRow, err := wfs.FindStepByID(ctx, in.StepID)
	if err != nil {
		return fmt.Errorf("EnterStep: lookup step %s: %w", in.StepID, err)
	}

	// Build the task pointer patch: status, current_step_id, and
	// (when caller asked for it) workflow_id. The metadata mutation
	// happens inside the closure so the whole thing lands in one tx.
	status := store.StatusWork
	patch := store.TaskPatch{
		Status:        &status,
		CurrentStepID: &stepRow.ID,
	}
	if in.WorkflowID != "" {
		patch.WorkflowID = &in.WorkflowID
	}

	_, err = tasks.UpdateMetadataAndPatch(ctx, in.TaskID, func(m map[string]any) error {
		var fired *MaxVisitsExceededError
		meta.MutateStepVisits(m, func(sv meta.StepVisits) {
			current := sv[stepRow.ID]
			next := current + 1
			if stepRow.MaxVisits > 0 && next > stepRow.MaxVisits {
				fired = &MaxVisitsExceededError{
					StepID:   stepRow.ID,
					StepName: stepRow.Name,
					Visits:   current,
					Max:      stepRow.MaxVisits,
				}
				return
			}
			sv[stepRow.ID] = next
		})
		if fired != nil {
			// Returning the error from fn aborts the tx without writing
			// the bumped counter OR the patch.
			return *fired
		}
		return nil
	}, patch)
	// The store may wrap fn's error (e.g. "update task: ...") in the
	// future; use errors.As so we recognise the cap error regardless of
	// wrapping.
	var mve MaxVisitsExceededError
	if errors.As(err, &mve) {
		return mve
	}
	if err != nil {
		return fmt.Errorf("EnterStep: %w", err)
	}
	return nil
}
