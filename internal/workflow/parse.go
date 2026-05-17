// Package workflow owns the workflows / steps / step_transitions tables
// and the JSON definition format consumed by `autosk workflow create
// --file`. See docs/plans/20260517-Workflows-Plan.md §§4.1 and §6.2.
package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// SyntheticPrefix is reserved for auto-generated `single:<agent>` workflows
// produced by EnsureSingle. User-defined workflows whose name starts with
// this prefix are rejected at parse time.
const SyntheticPrefix = "single:"

// Definition is the in-memory shape of a workflow JSON file. Steps are a
// map so callers can navigate by name; transition order within a step
// matches the source file order.
type Definition struct {
	Name        string
	Description string
	FirstStep   string
	Steps       map[string]StepDef
	// StepNames preserves source-file step order (helpful for stable
	// rendering / `workflow show` output).
	StepNames []string
}

// StepDef is one entry under "steps".
type StepDef struct {
	Agent       string
	NextSteps   []TransitionDef
}

// TransitionDef is one entry under "steps.<name>.next_steps". Exactly one
// of Step and TaskStatus is set; the parser enforces that.
type TransitionDef struct {
	Step       string // target step name
	TaskStatus string // one of human_feedback|done|cancelled
	PromptRule string
}

// IsTaskStatus reports whether this transition terminates the workflow
// (or routes to human_feedback) rather than advancing to a sibling step.
func (t TransitionDef) IsTaskStatus() bool { return t.TaskStatus != "" }

// rawDoc mirrors the on-disk shape one-to-one for json.Unmarshal.
type rawDoc struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	FirstStep   string                 `json:"first_step"`
	Steps       map[string]rawStep     `json:"steps"`
	// We capture the raw bytes so we can recover step order even though
	// map iteration is unordered.
	RawSteps json.RawMessage `json:"steps_raw,omitempty"`
}

type rawStep struct {
	Agent     string           `json:"agent"`
	NextSteps []rawTransition  `json:"next_steps"`
}

type rawTransition struct {
	Step       string `json:"step,omitempty"`
	TaskStatus string `json:"task_status,omitempty"`
	PromptRule string `json:"prompt_rule"`
}

// ParseFile is a convenience over ParseReader.
func ParseFile(path string) (Definition, error) {
	f, err := os.Open(path)
	if err != nil {
		return Definition{}, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	return ParseReader(f)
}

// ParseReader reads a JSON workflow definition from r and returns its
// in-memory form. Performs only structural / format checks; deeper
// validation (agent existence, reserved name, etc.) is in Validate.
func ParseReader(r io.Reader) (Definition, error) {
	body, err := io.ReadAll(r)
	if err != nil {
		return Definition{}, fmt.Errorf("read workflow: %w", err)
	}
	// First pass: standard unmarshal. The raw 'steps' field is also kept
	// (as json.RawMessage) so we can extract source-file step order.
	var raw rawDoc
	if err := json.Unmarshal(body, &raw); err != nil {
		return Definition{}, fmt.Errorf("decode workflow: %w", err)
	}

	def := Definition{
		Name:        strings.TrimSpace(raw.Name),
		Description: strings.TrimSpace(raw.Description),
		FirstStep:   strings.TrimSpace(raw.FirstStep),
		Steps:       make(map[string]StepDef, len(raw.Steps)),
	}
	for stepName, s := range raw.Steps {
		stepDef := StepDef{Agent: strings.TrimSpace(s.Agent)}
		for i, tr := range s.NextSteps {
			step := strings.TrimSpace(tr.Step)
			status := strings.TrimSpace(tr.TaskStatus)
			rule := strings.TrimSpace(tr.PromptRule)
			// Exactly-one constraint expressed here so Validate can focus
			// on referential checks.
			if (step == "") == (status == "") {
				return Definition{}, fmt.Errorf("step %q transition %d: exactly one of `step` or `task_status` must be set", stepName, i)
			}
			stepDef.NextSteps = append(stepDef.NextSteps, TransitionDef{
				Step:       step,
				TaskStatus: status,
				PromptRule: rule,
			})
		}
		def.Steps[stepName] = stepDef
	}

	// Recover step source order from the raw bytes.
	def.StepNames, err = recoverStepOrder(body)
	if err != nil {
		return Definition{}, err
	}
	return def, nil
}

// recoverStepOrder rescans the JSON body to extract the textual order of
// step names. This lets `workflow show` print steps the way the author
// wrote them rather than in random map order.
func recoverStepOrder(body []byte) ([]string, error) {
	var probe struct {
		Steps json.RawMessage `json:"steps"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return nil, fmt.Errorf("decode steps for order: %w", err)
	}
	dec := json.NewDecoder(strings.NewReader(string(probe.Steps)))
	tok, err := dec.Token()
	if err != nil {
		return nil, nil // empty / missing steps; Validate will complain
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return nil, errors.New("`steps` must be an object")
	}
	var names []string
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("decode step name: %w", err)
		}
		key, ok := t.(string)
		if !ok {
			return nil, fmt.Errorf("step name token: %v", t)
		}
		names = append(names, key)
		// Skip the step's value (an object).
		if err := skipValue(dec); err != nil {
			return nil, err
		}
	}
	return names, nil
}

// skipValue advances the decoder past one JSON value. Handles nested
// objects and arrays.
func skipValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if delim, ok := tok.(json.Delim); ok {
		// Balance braces/brackets.
		depth := 1
		for depth > 0 {
			t, err := dec.Token()
			if err != nil {
				return err
			}
			if d, ok := t.(json.Delim); ok {
				switch d {
				case '{', '[':
					depth++
				case '}', ']':
					depth--
				}
			}
			_ = delim
		}
	}
	return nil
}
