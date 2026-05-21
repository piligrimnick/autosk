package main

import "autosk/internal/workflow"

// mapEnterStepError is a CLI-side shim over workflow.MapEnterStepError
// so callers in this package keep the existing call shape
// `mapEnterStepError(err, taskID)`. The actual hint construction —
// including the `autosk metadata reset-visits …` copy-paste — lives in
// internal/workflow so the lazy TUI and the CLI render the same
// message and chain (see workflow.EnterStepHint).
func mapEnterStepError(err error, taskID string) error {
	return workflow.MapEnterStepError(taskID, err)
}
