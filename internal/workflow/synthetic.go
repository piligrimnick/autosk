package workflow

import (
	"context"
	"fmt"
)

// SyntheticName returns the canonical workflow name for the single-agent
// sugar form `--agent <name>`. See plan §5.5.
func SyntheticName(agentName string) string {
	return SyntheticPrefix + agentName
}

// EnsureSingle returns the workflow named `single:<agentName>`, creating
// it transactionally if absent. Idempotent — safe to call concurrently
// (UNIQUE(name) gives one writer the row, others fall through to the
// GetByName branch).
//
// Shape (matches plan §5.5):
//
//	name:        "single:<agent>"
//	first_step:  "do"
//	is_synthetic: 1
//	steps:
//	  do:
//	    agent: <agent>
//	    next_steps:
//	      - { task_status: done,           prompt_rule: ... }
//	      - { task_status: cancelled,      prompt_rule: ... }
//	      - { task_status: human_feedback, prompt_rule: ... }
func (s *Store) EnsureSingle(ctx context.Context, agentName string) (Workflow, error) {
	if agentName == "" {
		return Workflow{}, fmt.Errorf("agent name is empty")
	}
	name := SyntheticName(agentName)
	if w, err := s.GetByName(ctx, name); err == nil {
		return w, nil
	} else if err != ErrNotFound {
		return Workflow{}, err
	}
	def := Definition{
		Name:        name,
		Description: fmt.Sprintf("Auto-generated single-agent workflow for %s.", agentName),
		FirstStep:   "do",
		Steps: map[string]StepDef{
			"do": {
				AgentName: agentName,
				NextSteps: []TransitionDef{
					{TaskStatus: "done", PromptRule: "When the work is complete."},
					{TaskStatus: "cancelled", PromptRule: "When the task cannot be completed."},
					{TaskStatus: "human_feedback", PromptRule: "When you need a human decision or input."},
				},
			},
		},
		StepNames: []string{"do"},
	}
	w, err := s.Create(ctx, def, true /*isSynthetic*/)
	if err == nil {
		return w, nil
	}
	// Race: another writer beat us to UNIQUE(name). Read back.
	if w2, err2 := s.GetByName(ctx, name); err2 == nil {
		return w2, nil
	}
	return Workflow{}, err
}
