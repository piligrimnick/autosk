package workflow_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"autosk/internal/workflow"
)

func TestParseFile_Example(t *testing.T) {
	// The repo ships an example; round-trip through ParseFile.
	def, err := workflow.ParseFile("../../docs/notes/workflow-example.json")
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if def.Name != "feature-dev" {
		t.Errorf("name: %q", def.Name)
	}
	if def.FirstStep != "dev" {
		t.Errorf("first_step: %q", def.FirstStep)
	}
	if len(def.Steps) != 3 {
		t.Errorf("steps count: %d", len(def.Steps))
	}
	if got := def.StepNames; len(got) != 3 || got[0] != "dev" || got[1] != "review" || got[2] != "validator" {
		t.Errorf("step order: %v", got)
	}
	val, ok := def.Steps["validator"]
	if !ok {
		t.Fatal("missing validator")
	}
	if len(val.NextSteps) != 2 {
		t.Fatalf("validator transitions: %d", len(val.NextSteps))
	}
	gotStatus := val.NextSteps[1].TaskStatus
	if gotStatus != "human_feedback" {
		t.Errorf("validator's second transition task_status: %q", gotStatus)
	}
}

func TestParse_ExactlyOneTarget(t *testing.T) {
	body := `{
		"name": "x", "first_step": "a",
		"steps": {
			"a": {"agent": "dev",
			      "next_steps": [{"step": "a", "task_status": "done", "prompt_rule": "..."}]}
		}}`
	_, err := workflow.ParseReader(strings.NewReader(body))
	if err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("want exactly-one error, got %v", err)
	}
}

func TestValidate_Structural(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name    string
		body    string
		wantErr string
	}{
		{
			name:    "missing first_step",
			body:    `{"name":"x","steps":{"a":{"agent":"dev","next_steps":[{"task_status":"done","prompt_rule":"."}]}}}`,
			wantErr: "first_step",
		},
		{
			name:    "first_step not in steps",
			body:    `{"name":"x","first_step":"b","steps":{"a":{"agent":"dev","next_steps":[{"task_status":"done","prompt_rule":"."}]}}}`,
			wantErr: "first_step",
		},
		{
			name:    "transition to unknown step",
			body:    `{"name":"x","first_step":"a","steps":{"a":{"agent":"dev","next_steps":[{"step":"b","prompt_rule":"."}]}}}`,
			wantErr: "target step",
		},
		{
			name:    "bad task_status",
			body:    `{"name":"x","first_step":"a","steps":{"a":{"agent":"dev","next_steps":[{"task_status":"deleted","prompt_rule":"."}]}}}`,
			wantErr: "task_status",
		},
		{
			name:    "missing prompt_rule",
			body:    `{"name":"x","first_step":"a","steps":{"a":{"agent":"dev","next_steps":[{"task_status":"done","prompt_rule":""}]}}}`,
			wantErr: "prompt_rule",
		},
		{
			name:    "reserved prefix",
			body:    `{"name":"single:dev","first_step":"a","steps":{"a":{"agent":"dev","next_steps":[{"task_status":"done","prompt_rule":"."}]}}}`,
			wantErr: "reserved prefix",
		},
		{
			name:    "step with no transitions",
			body:    `{"name":"x","first_step":"a","steps":{"a":{"agent":"dev","next_steps":[]}}}`,
			wantErr: "at least one transition",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			def, err := workflow.ParseReader(strings.NewReader(c.body))
			if err != nil {
				// Parser-level errors are also acceptable (e.g.
				// exactly-one); they're caught at parse time so
				// validate isn't reached.
				if strings.Contains(err.Error(), c.wantErr) {
					return
				}
				t.Fatalf("parse: %v", err)
			}
			err = workflow.Validate(ctx, def, nil, workflow.ValidateOpts{})
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("want error containing %q, got %v", c.wantErr, err)
			}
		})
	}
}

func TestValidate_AgentMissing(t *testing.T) {
	t.Skip("requires real agent store; covered in store_test.go via Create round-trip")
	_ = filepath.Separator // pacify unused-imports for the skip path
	_ = os.PathSeparator
}
