package workflow

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"autosk/internal/agent"
	"autosk/internal/id"
)

// ID prefixes for workflow/step rows.
const (
	WorkflowIDPrefix = "wf"
	StepIDPrefix     = "st"
)

// Sentinel errors.
var (
	ErrNotOpen        = errors.New("workflow store: not open")
	ErrNotFound       = errors.New("workflow not found")
	ErrAlreadyExist   = errors.New("workflow already exists")
	ErrInUse          = errors.New("workflow has tasks pointing at it; refuse delete")
)

// Workflow is the materialised view of one `workflows` row.
type Workflow struct {
	ID          string
	Name        string
	Description string
	FirstStepID string
	IsSynthetic bool
	CreatedAt   time.Time
	Steps       []Step // populated by GetByName/Show, not by List
}

// Step mirrors one `steps` row plus its outgoing transitions.
type Step struct {
	ID          string
	WorkflowID  string
	Name        string
	AgentID     string
	AgentName   string         // joined-in for convenience
	Transitions []Transition   // outgoing, in source order
}

// Transition mirrors one `step_transitions` row.
type Transition struct {
	ID         int64
	StepID     string
	NextStepID string // empty when TaskStatus is set
	TaskStatus string // empty when NextStepID is set
	PromptRule string
	// NextStepName populated as a convenience when reading via the store.
	NextStepName string
}

// IsTaskStatus reports whether this transition terminates / parks the
// workflow rather than advancing to a sibling step.
func (t Transition) IsTaskStatus() bool { return t.TaskStatus != "" }

// Store backs workflows / steps / step_transitions on the shared *sql.DB.
type Store struct {
	db    *sql.DB
	agent *agent.Store // resolves agent names → ids during Create
}

// New constructs a Store. agent_store must be non-nil (we need it for
// agent name → id resolution).
func New(db *sql.DB, ag *agent.Store) *Store {
	return &Store{db: db, agent: ag}
}

// Create persists a parsed Definition transactionally. Returns the
// materialised Workflow with all nested steps + transitions.
//
// On UNIQUE(name) collision returns ErrAlreadyExist.
func (s *Store) Create(ctx context.Context, def Definition, isSynthetic bool) (Workflow, error) {
	if s.db == nil {
		return Workflow{}, ErrNotOpen
	}
	if err := Validate(ctx, def, s.agent, ValidateOpts{AllowSyntheticName: isSynthetic}); err != nil {
		return Workflow{}, err
	}
	// Resolve agent names to ids up-front so we fail fast.
	stepAgents := make(map[string]string, len(def.Steps))
	for stepName, sd := range def.Steps {
		a, err := s.agent.GetByName(ctx, sd.Agent)
		if err != nil {
			return Workflow{}, fmt.Errorf("resolve agent %q for step %q: %w", sd.Agent, stepName, err)
		}
		stepAgents[stepName] = a.ID
	}

	// Mint ids BEFORE BeginTx: doltlite is single-writer (one conn), so
	// queries inside a tx must go through *Tx, not *DB. id.NewUnique uses
	// the *DB-level helpers, so we run it first.
	wfID, err := id.NewUnique(WorkflowIDPrefix, func(candidate string) (bool, error) {
		return s.workflowIDExists(ctx, candidate)
	})
	if err != nil {
		return Workflow{}, fmt.Errorf("generate workflow id: %w", err)
	}
	stepIDs := make(map[string]string, len(def.Steps))
	for _, name := range orderedStepNames(def) {
		stepID, err := id.NewUnique(StepIDPrefix, func(candidate string) (bool, error) {
			return s.stepIDExists(ctx, candidate)
		})
		if err != nil {
			return Workflow{}, fmt.Errorf("generate step id: %w", err)
		}
		stepIDs[name] = stepID
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Workflow{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().Unix()

	firstStepID, ok := stepIDs[def.FirstStep]
	if !ok {
		return Workflow{}, fmt.Errorf("internal: first_step %q has no id", def.FirstStep)
	}

	synthetic := 0
	if isSynthetic {
		synthetic = 1
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO workflows(id, name, description, first_step_id, is_synthetic, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, wfID, def.Name, def.Description, firstStepID, synthetic, now); err != nil {
		if isUniqueErr(err, "workflows.name") {
			return Workflow{}, fmt.Errorf("%w: %s", ErrAlreadyExist, def.Name)
		}
		return Workflow{}, fmt.Errorf("insert workflow: %w", err)
	}

	// Two passes: insert ALL steps first (so forward-referencing
	// transitions like dev→review don't hit FK), then insert transitions.
	names := orderedStepNames(def)
	for seq, stepName := range names {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO steps(id, workflow_id, name, agent_id, seq) VALUES (?, ?, ?, ?, ?)
		`, stepIDs[stepName], wfID, stepName, stepAgents[stepName], seq); err != nil {
			return Workflow{}, fmt.Errorf("insert step %q: %w", stepName, err)
		}
	}
	for _, stepName := range names {
		sd := def.Steps[stepName]
		for i, tr := range sd.NextSteps {
			var nextID, status any
			if tr.IsTaskStatus() {
				nextID = nil
				status = tr.TaskStatus
			} else {
				nid, ok := stepIDs[tr.Step]
				if !ok {
					return Workflow{}, fmt.Errorf("internal: transition %d in step %q targets unknown step %q", i, stepName, tr.Step)
				}
				nextID = nid
				status = nil
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO step_transitions(step_id, next_step_id, task_status, prompt_rule)
				VALUES (?, ?, ?, ?)
			`, stepIDs[stepName], nextID, status, tr.PromptRule); err != nil {
				return Workflow{}, fmt.Errorf("insert transition %d for step %q: %w", i, stepName, err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return Workflow{}, fmt.Errorf("commit: %w", err)
	}
	return s.GetByName(ctx, def.Name)
}

// GetByName returns a fully-loaded workflow (with steps + transitions),
// or ErrNotFound.
func (s *Store) GetByName(ctx context.Context, name string) (Workflow, error) {
	if s.db == nil {
		return Workflow{}, ErrNotOpen
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT id, name, description, first_step_id, is_synthetic, created_at
		   FROM workflows WHERE name = ?`, name)
	w, err := scanWorkflowRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Workflow{}, ErrNotFound
	}
	if err != nil {
		return Workflow{}, err
	}
	steps, err := s.loadSteps(ctx, w.ID)
	if err != nil {
		return Workflow{}, fmt.Errorf("load steps: %w", err)
	}
	w.Steps = steps
	return w, nil
}

// List returns workflows ordered by name. is_synthetic=1 rows are hidden
// unless includeSynthetic is true.
func (s *Store) List(ctx context.Context, includeSynthetic bool) ([]Workflow, error) {
	if s.db == nil {
		return nil, ErrNotOpen
	}
	q := `SELECT id, name, description, first_step_id, is_synthetic, created_at
	        FROM workflows`
	if !includeSynthetic {
		q += ` WHERE is_synthetic = 0`
	}
	q += ` ORDER BY name ASC`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list workflows: %w", err)
	}
	defer rows.Close()
	var out []Workflow
	for rows.Next() {
		w, err := scanWorkflowRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// Delete removes a workflow by name. Refuses with ErrInUse when at least
// one task currently references it.
func (s *Store) Delete(ctx context.Context, name string) error {
	if s.db == nil {
		return ErrNotOpen
	}
	w, err := s.GetByName(ctx, name)
	if err != nil {
		return err
	}
	var refs int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tasks WHERE workflow_id = ?`, w.ID).Scan(&refs); err != nil {
		return fmt.Errorf("count tasks: %w", err)
	}
	if refs > 0 {
		return fmt.Errorf("%w: %d task(s) reference %q", ErrInUse, refs, name)
	}
	// CASCADE handles steps + step_transitions; rows in daemon_runs use
	// ON DELETE RESTRICT on step_id, so if any run row exists deletion
	// fails noisily. That's the safe default for v0.2.
	if _, err := s.db.ExecContext(ctx, `DELETE FROM workflows WHERE id = ?`, w.ID); err != nil {
		return fmt.Errorf("delete workflow: %w", err)
	}
	return nil
}

// FindStepByID returns the step row for the given step id, including its
// outgoing transitions. Used by the CLI to surface derived current_step
// / current_agent on a Task.
func (s *Store) FindStepByID(ctx context.Context, stepID string) (Step, error) {
	if s.db == nil {
		return Step{}, ErrNotOpen
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT s.id, s.workflow_id, s.name, s.agent_id, a.name
		  FROM steps s JOIN agents a ON s.agent_id = a.id
		 WHERE s.id = ?`, stepID)
	var st Step
	if err := row.Scan(&st.ID, &st.WorkflowID, &st.Name, &st.AgentID, &st.AgentName); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Step{}, ErrNotFound
		}
		return Step{}, err
	}
	trs, err := s.loadTransitions(ctx, st.ID)
	if err != nil {
		return Step{}, err
	}
	st.Transitions = trs
	return st, nil
}

// GetByID returns a fully-loaded workflow by id, mirror of GetByName.
func (s *Store) GetByID(ctx context.Context, workflowID string) (Workflow, error) {
	if s.db == nil {
		return Workflow{}, ErrNotOpen
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT id, name, description, first_step_id, is_synthetic, created_at
		   FROM workflows WHERE id = ?`, workflowID)
	w, err := scanWorkflowRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Workflow{}, ErrNotFound
	}
	if err != nil {
		return Workflow{}, err
	}
	steps, err := s.loadSteps(ctx, w.ID)
	if err != nil {
		return Workflow{}, err
	}
	w.Steps = steps
	return w, nil
}

// FindStepByName returns the (single) step row matching (workflow, step name).
// Used by EnsureSingle and the CLI to translate step names into ids.
func (s *Store) FindStepByName(ctx context.Context, workflowID, stepName string) (Step, error) {
	if s.db == nil {
		return Step{}, ErrNotOpen
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT s.id, s.workflow_id, s.name, s.agent_id, a.name
		  FROM steps s JOIN agents a ON s.agent_id = a.id
		 WHERE s.workflow_id = ? AND s.name = ?`,
		workflowID, stepName)
	var st Step
	if err := row.Scan(&st.ID, &st.WorkflowID, &st.Name, &st.AgentID, &st.AgentName); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Step{}, ErrNotFound
		}
		return Step{}, err
	}
	trs, err := s.loadTransitions(ctx, st.ID)
	if err != nil {
		return Step{}, err
	}
	st.Transitions = trs
	return st, nil
}

// ---- helpers --------------------------------------------------------------

func (s *Store) workflowIDExists(ctx context.Context, wfID string) (bool, error) {
	var x int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM workflows WHERE id = ?`, wfID).Scan(&x)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (s *Store) stepIDExists(ctx context.Context, stID string) (bool, error) {
	var x int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM steps WHERE id = ?`, stID).Scan(&x)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (s *Store) loadSteps(ctx context.Context, workflowID string) ([]Step, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT s.id, s.workflow_id, s.name, s.agent_id, a.name
		  FROM steps s JOIN agents a ON s.agent_id = a.id
		 WHERE s.workflow_id = ?
		 ORDER BY s.seq ASC`, workflowID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Step
	for rows.Next() {
		var st Step
		if err := rows.Scan(&st.ID, &st.WorkflowID, &st.Name, &st.AgentID, &st.AgentName); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		trs, err := s.loadTransitions(ctx, out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Transitions = trs
	}
	return out, nil
}

func (s *Store) loadTransitions(ctx context.Context, stepID string) ([]Transition, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT t.id, t.step_id, t.next_step_id, t.task_status, t.prompt_rule,
		       (SELECT name FROM steps WHERE id = t.next_step_id) AS next_name
		  FROM step_transitions t
		 WHERE t.step_id = ?
		 ORDER BY t.id ASC`, stepID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Transition
	for rows.Next() {
		var (
			tr        Transition
			nextID    sql.NullString
			status    sql.NullString
			nextName  sql.NullString
		)
		if err := rows.Scan(&tr.ID, &tr.StepID, &nextID, &status, &tr.PromptRule, &nextName); err != nil {
			return nil, err
		}
		tr.NextStepID = nextID.String
		tr.TaskStatus = status.String
		tr.NextStepName = nextName.String
		out = append(out, tr)
	}
	return out, rows.Err()
}

func scanWorkflowRow(sc interface{ Scan(...any) error }) (Workflow, error) {
	var (
		w       Workflow
		synth   int
		created int64
	)
	if err := sc.Scan(&w.ID, &w.Name, &w.Description, &w.FirstStepID, &synth, &created); err != nil {
		return Workflow{}, err
	}
	w.IsSynthetic = synth != 0
	w.CreatedAt = time.Unix(created, 0).UTC()
	return w, nil
}

func isUniqueErr(err error, indexKey string) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "UNIQUE constraint failed: "+indexKey)
}

// orderedStepNames returns the steps' source-file order (if known) or a
// stable alphabetical order otherwise. Centralised here so Create and
// future renderers agree on ordering.
func orderedStepNames(def Definition) []string {
	if len(def.StepNames) > 0 {
		return def.StepNames
	}
	out := make([]string, 0, len(def.Steps))
	for k := range def.Steps {
		out = append(out, k)
	}
	// Stable sort for determinism (alpha asc).
	sortStringsInPlace(out)
	return out
}

// sortStringsInPlace is a tiny inline sort to avoid adding sort import
// solely for the fallback path.
func sortStringsInPlace(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
