package executor

import (
	"encoding/json"
	"fmt"

	"autosk/internal/agent/pkgregistry"
	"autosk/internal/store"
	"autosk/internal/workflow"
)

// SeedSchemaVersion is the version of the RunContextSeed payload that
// the bootstrapper expects. Bumped on breaking changes to the JSON
// shape. The bootstrapper rejects mismatches.
const SeedSchemaVersion = 1

// taskSeed is the JSON view of the task fields a custom runner needs.
type taskSeed struct {
	ID            string `json:"id"`
	Title         string `json:"title"`
	Description   string `json:"description"`
	Status        string `json:"status"`
	Priority      int    `json:"priority"`
	WorkflowID    string `json:"workflow_id"`
	CurrentStepID string `json:"current_step_id"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
}

type stepSeed struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Agent string `json:"agent"`
}

type workflowSeed struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

type transitionSeed struct {
	Kind       string `json:"kind"`   // "step" | "task_status"
	Target     string `json:"target"` // step name or task_status value
	PromptRule string `json:"prompt_rule"`
}

type commentSeed struct {
	Line string `json:"line"`
}

// RunContextSeed is the JSON payload written to the bootstrapper's
// stdin. The TS-side `RunContext` type in @autosk/agent-sdk mirrors
// this shape (minus the function members which the bootstrapper
// constructs).
type RunContextSeed struct {
	SchemaVersion int              `json:"schema_version"`
	Task          taskSeed         `json:"task"`
	Step          stepSeed         `json:"step"`
	Workflow      workflowSeed     `json:"workflow"`
	Comments      []commentSeed    `json:"comments"`
	Transitions   []transitionSeed `json:"transitions"`
	ProjectRoot   string           `json:"project_root"`
	JobID         string           `json:"job_id"`
	AgentName     string           `json:"agent_name"`
	AgentVersion  string           `json:"agent_version"`
	AgentInstall  string           `json:"agent_install"`
}

// RenderSeedJSON builds the JSON-encoded RunContextSeed sent to a
// custom runner's stdin.
//
// The shape is deliberately close to RenderPrompt's input so the
// bootstrapper / runner has the same view as a pi-based agent would
// from the rendered prompt.
func RenderSeedJSON(
	t store.Task,
	wf workflow.Workflow,
	stepRow workflow.Step,
	cfg pkgregistry.PackageConfig,
	commentLines []string,
	jobID string,
) (string, error) {
	seed := RunContextSeed{
		SchemaVersion: SeedSchemaVersion,
		Task: taskSeed{
			ID:            t.ID,
			Title:         t.Title,
			Description:   t.Description,
			Status:        string(t.Status),
			Priority:      t.Priority,
			WorkflowID:    t.WorkflowID,
			CurrentStepID: t.CurrentStepID,
			CreatedAt:     t.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
			UpdatedAt:     t.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		},
		Step: stepSeed{
			ID:    stepRow.ID,
			Name:  stepRow.Name,
			Agent: stepRow.AgentName,
		},
		Workflow: workflowSeed{
			ID:          wf.ID,
			Name:        wf.Name,
			Description: wf.Description,
		},
		ProjectRoot:  "", // executor fills in via cwd before render; bootstrapper resolves from cwd
		JobID:        jobID,
		AgentName:    cfg.Name,
		AgentVersion: cfg.Version,
		AgentInstall: cfg.InstallDir,
	}
	for _, tr := range stepRow.Transitions {
		var t transitionSeed
		if tr.IsTaskStatus() {
			t = transitionSeed{Kind: "task_status", Target: tr.TaskStatus, PromptRule: tr.PromptRule}
		} else {
			t = transitionSeed{Kind: "step", Target: tr.NextStepName, PromptRule: tr.PromptRule}
		}
		seed.Transitions = append(seed.Transitions, t)
	}
	for _, line := range commentLines {
		seed.Comments = append(seed.Comments, commentSeed{Line: line})
	}
	b, err := json.Marshal(seed)
	if err != nil {
		return "", fmt.Errorf("marshal seed: %w", err)
	}
	// Trailing newline so the bootstrapper's line-oriented reader (if
	// it uses one) doesn't block.
	return string(b) + "\n", nil
}
