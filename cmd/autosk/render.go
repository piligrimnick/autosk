package main

import (
	"context"
	"os"

	"autosk/internal/render"
	"autosk/internal/store"
	"autosk/internal/workflow"
)

// This file hosts the small render helpers shared across enroll /
// resume / create / show. They used to live in assign.go alongside
// the (now-deleted) `autosk assign` verb; the verb was a duplicate
// of `enroll --agent`, but the renderers were always cross-cutting
// helpers that several call sites depended on. See the plan in
// docs/plans/2026052*-Remove-Assign-Open-Enroll.md for context.

// emitRich prints a task with derived workflow / step / author info
// resolved against the provided workflow store + an agent store derived
// from it. Best-effort: lookup failures degrade to bare ids.
func emitRich(ctx context.Context, wfs *workflow.Store, t store.Task) error {
	if flagQuiet {
		return nil
	}
	opts := taskRenderOpts(ctx, wfs, t)
	if flagJSON {
		return render.TaskJSONTo(os.Stdout, t, opts...)
	}
	return render.Task(os.Stdout, t, opts...)
}

// emitRichWithBlocked is emitRich plus the derived `blocked` /
// `blocked_by` / `blocks` fields pulled from the store. This is what
// `show --json` emits, and `enroll --json` promises shape parity with
// it (plan acceptance §5). Best-effort: a dep-lookup failure surfaces
// as a non-fatal warning and the call falls back to emitRich.
func emitRichWithBlocked(ctx context.Context, wfs *workflow.Store, s store.Store, t store.Task) error {
	if flagQuiet {
		return nil
	}
	opts := taskRenderOpts(ctx, wfs, t)
	incoming, outgoing, derr := s.Deps(ctx, t.ID)
	blocked, berr := s.IsBlocked(ctx, t.ID)
	if derr == nil && berr == nil {
		opts = append(opts, render.WithBlocked(blocked, incoming, outgoing))
	}
	if flagJSON {
		return render.TaskJSONTo(os.Stdout, t, opts...)
	}
	return render.Task(os.Stdout, t, opts...)
}

// taskRenderOpts builds the standard set of render.Options for a task:
// derived current_step + current_agent (from the step) and the
// human-friendly names for workflow + author.
func taskRenderOpts(ctx context.Context, wfs *workflow.Store, t store.Task) []render.Option {
	var opts []render.Option
	if t.CurrentStepID != "" {
		if step, err := wfs.FindStepByID(ctx, t.CurrentStepID); err == nil {
			opts = append(opts, render.WithStep(step.Name, step.AgentName, step.AgentID))
		}
	}
	if t.WorkflowID != "" {
		if w, err := wfs.GetByID(ctx, t.WorkflowID); err == nil {
			opts = append(opts, render.WithWorkflow(w.Name))
		}
	}
	if t.AuthorID != "" {
		if ag := wfs.Agents(); ag != nil {
			if a, err := ag.GetByID(ctx, t.AuthorID); err == nil {
				opts = append(opts, render.WithAuthor(a.Name))
			}
		}
	}
	return opts
}
