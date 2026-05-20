# Step visit limits + task metadata — plan

**Status:** plan (not yet started).
**Date:** 2026-05-20.
**Owners:** autosk core.
**Related code:** `internal/workflow/{parse,validate,store}.go`,
`internal/migrations/`, `internal/store/{store.go,doltlite/}`,
`internal/daemon/executor/executor.go`,
`cmd/autosk/{create,enroll,resume,update,show,step}.go`,
`internal/render/`.

---

## 1. Motivation

Workflows like `dev → review → validator → dev → review → …` can
loop indefinitely if the agents keep bouncing the task back. Today
nothing caps that: a misbehaving (or just stuck) agent can churn the
same step forever, burn tokens, and never surface for a human. We
need a way to:

1. Declare a per-step ceiling on how many times one task may enter
   that step.
2. Have the engine enforce it: an over-the-limit entry fails the
   `daemon_runs` row and parks the task to `human_feedback` with a
   clear cause.
3. Persist per-task visit counters so the engine can decide, and so a
   human can inspect why a task was parked.
4. Expose a small CLI for the human to read, edit, and reset task
   metadata (incl. the visit counters), as the only escape hatch when
   a task hits the ceiling but should keep going.

---

## 2. Design at a glance (chosen options)

These were settled with the user; do not relitigate.

| Decision | Chosen | Notes |
|---|---|---|
| Where the limit lives in JSON | `max_visits: N` directly on the step block. | No `limits` wrapper today; can add one later if more per-step limits appear. |
| What counts as a visit | Every transition INTO the step, including the very first one (task creation / `enroll`). | `resume` *without* `--to` does NOT count (no transition). |
| Storage for counters | New `tasks.metadata TEXT` JSON blob. Counters live under a reserved key `step_visits` keyed by `step_id`. | One column, one source of truth. No separate counters table. |
| Counter key shape | `metadata.step_visits[<step_id>] = int` | `step_id` is globally unique; if a workflow is dropped + re-created the new step ids are fresh and old counters become orphans (acceptable — they’re ignored). |
| Automatic resets | None. Counters are sticky; only humans clear them via CLI. | This matches "fail-loud, escalate to human" intent. |
| CLI surface | New verb `autosk metadata {show,set,unset,reset-visits}`. | `update` is left alone; `show` is extended to surface visit counters. |
| Failure mode | Hard fail. `daemon_runs.status=failed`, `error='step_max_visits_exceeded'`, task → `human_feedback`, `current_step_id` **preserved**. | Reuses existing `parkTaskOnFailure`. No agent kickback, no auto-comment. |

The deliberate non-features (out of scope for this plan):

- A workflow-level `default_max_visits` fallback.
- Auto-resets on `reopen` / `enroll` into a new workflow.
- A pre-emptive corrective kickback ("you're at 4/5, exit cleanly").
- Showing visit count in the agent's prompt.
- Lazy TUI integration (see §10 for follow-up only).
- A `single:<agent>` shorthand for max_visits on synthetic workflows.

---

## 3. Schema changes (migration `004_step_max_visits_and_task_metadata.sql`)

Single SQL file, two ALTERs:

```sql
-- 004_step_max_visits_and_task_metadata.sql
--
-- Per-step `max_visits` cap and per-task `metadata` JSON blob.
-- 0 (the column default) means "unlimited" — preserving prior
-- behaviour for every existing step row.
--
-- metadata is a free-form JSON object; the engine reserves the
-- top-level key `step_visits` for per-step counters keyed by step_id.

ALTER TABLE steps  ADD COLUMN max_visits INTEGER NOT NULL DEFAULT 0
       CHECK (max_visits >= 0);
ALTER TABLE tasks  ADD COLUMN metadata   TEXT;
```

Notes:

- `metadata` is nullable; NULL == `{}` for all practical purposes.
  The code path always reads as `{}` when NULL and only persists a
  non-empty object on write (so an empty metadata round-trips back
  to NULL — keeps diffs tidy).
- `max_visits` is `NOT NULL DEFAULT 0` so existing `steps` rows
  upgrade cleanly. `0` = no limit (documented loudly in
  `docs/workflows.md`).
- No new indexes. The visit counter is read/written per task on hot
  path; that path always already has the row in hand (the executor
  fetches the task right before checking).
- `migrations.go` gains a new step running this file. `CheckV1` and
  the schema-version probe pick up the bump automatically; no other
  bootstrap logic changes.

### 3.1 What about `daemon_runs`?

No schema change. The visit cap is enforced *before* a transition
into the step takes effect (so before a new `daemon_runs` row is
ever picked up); when a check fails mid-`Run`, the existing
`MarkFailed(...)` + `parkTaskOnFailure(...)` path is sufficient. We
just feed it a new error string.

---

## 4. Workflow JSON shape

### 4.1 Accepted shape

```jsonc
{
  "name": "feature-dev",
  "first_step": "dev",
  "steps": {
    "dev": {
      "agent":       { "name": "@autosk/developer" },
      "max_visits":  5,
      "next_steps":  [{ "step": "review", "prompt_rule": "..." }]
    },
    "review": {
      "agent":       { "name": "@autosk/code-reviewer" },
      "max_visits":  5,
      "next_steps":  [
        { "step": "validator", "prompt_rule": "..." },
        { "step": "dev",       "prompt_rule": "..." }
      ]
    },
    "validator": {
      "agent":       { "name": "@autosk/task-validator" },
      "next_steps":  [
        { "step": "dev",                   "prompt_rule": "..." },
        { "task_status": "human_feedback", "prompt_rule": "..." }
      ]
    }
  }
}
```

`max_visits` is optional. Absent / `0` → unlimited (no cap, no
counter increment skipped — the counter still runs, but the engine
never compares it to anything).

### 4.2 Parser changes (`internal/workflow/parse.go`)

- Add `MaxVisits int` to `rawStep` and `StepDef`. Keep JSON tag
  `max_visits`.
- `StepDef.MaxVisits` reads through `parseReader` like any other
  field. Default zero.
- Per-step unknown-key rejection currently only applies to
  `agent.params` (via `allowedParamKeys`). The step block itself is
  decoded with the bog-standard `json.Unmarshal`, which silently
  ignores unknown keys; we leave that as-is (adding strict mode to
  the step block is a separate cleanup).
- `Validate` (`internal/workflow/validate.go`): reject negative
  `max_visits` with `step %q: max_visits must be >= 0` even though
  the DB CHECK would also catch it — fail-fast at import time gives
  a better error.
- Add a sanity rule: if `first_step` has `max_visits == 1` it is
  allowed (the very first entry consumes the one allowance). If
  someone writes `max_visits == 0` on `first_step` that's "unlimited"
  and trivially fine; we don't special-case anything.

### 4.3 Store + scan changes (`internal/workflow/store.go`)

- Extend `Step` struct: `MaxVisits int`.
- Update every `SELECT … FROM steps …` to include `s.max_visits`:
  `loadSteps`, `FindStepByID`, `FindStepByName`. Update `scanStep`
  signature to read the extra column.
- `Create(...)`: `INSERT INTO steps(... , max_visits)` carries
  `sd.MaxVisits` (defaults to 0).
- `EnsureSingle(...)` (in `internal/workflow/synthetic.go`): pass
  `max_visits = 0`. Single-agent flows stay uncapped.

### 4.4 Docs

`docs/workflows.md` gets a new subsection under **Workflow** describing
`max_visits` semantics, including:

- "every transition INTO this step counts; that includes the first
  entry when a task is created with this workflow"
- "`autosk resume <id>` (no `--to`) does NOT count; `--to STEP` to
  the same step that the task currently sits on does (it’s a
  transition)"
- "0 / absent = unlimited"
- "when a task hits the cap, the run is failed with
  `step_max_visits_exceeded`, the task is parked to
  `human_feedback`, and `current_step_id` is preserved so
  `autosk resume` returns you to the same step once you've reset the
  counter"
- pointer at `autosk metadata reset-visits` as the escape hatch

---

## 5. Task `metadata` infrastructure

### 5.1 In-memory shape

`store.Task` gains:

```go
type Task struct {
    // ... existing fields ...
    Metadata map[string]any
}
```

Reasoning: free-form `map[string]any` makes the field forward-compatible
and trivially round-trippable to JSON. The reserved `step_visits`
sub-key has a typed accessor (see §5.3) so the engine never reaches
in with magic strings.

### 5.2 Patch shape

`store.TaskPatch` gains:

```go
type TaskPatch struct {
    // ... existing fields ...
    Metadata *map[string]any // nil = unchanged, non-nil = REPLACE wholesale
}
```

Replacement semantics keep `UpdateTask` simple. Any partial-edit
behaviour (set a single key, unset a single key) is done by the
caller: load → mutate the map → patch with the mutated map. The CLI
in §7 does exactly that, and so does the engine's increment path.

We also add a single mutex-style helper in `store/doltlite` for the
"read-modify-write" pattern used by both the engine and the CLI:

```go
// UpdateMetadata applies fn to a copy of the task's current metadata
// inside a single tx; persists the result. Returns the new map.
// nil-or-empty map after fn → metadata column written as SQL NULL.
func (s *Store) UpdateMetadata(ctx context.Context, taskID string,
    fn func(m map[string]any) error) (map[string]any, error)
```

This is implemented as a `BEGIN IMMEDIATE` SELECT + UPDATE so two
concurrent callers don't clobber each other. (Doltlite is
single-writer; the helper still serializes via the existing write
mutex — see `doltlite.Store.DB()`.)

`store.Store` interface gains `UpdateMetadata(ctx, taskID, fn) error`.
`doltserver` backend (stub) returns `errors.ErrUnsupported`.

### 5.3 Typed accessor for the reserved section

A small package `internal/meta` (new) provides typed read/write of
the reserved `step_visits` namespace so neither the engine nor the
CLI parses raw maps:

```go
package meta

// Key for the reserved sub-object inside Task.Metadata.
const StepVisitsKey = "step_visits"

// StepVisits is a map step_id → visit count (>= 0).
type StepVisits map[string]int

func GetStepVisits(m map[string]any) StepVisits { /* ... */ }
func SetStepVisits(m map[string]any, sv StepVisits) { /* ... */ }
// MutateStepVisits is the convenience wrapper used by the engine.
func MutateStepVisits(m map[string]any, fn func(StepVisits)) {
    sv := GetStepVisits(m)
    fn(sv)
    if len(sv) == 0 {
        delete(m, StepVisitsKey)
        return
    }
    SetStepVisits(m, sv)
}
```

`GetStepVisits` tolerates the wire shape produced by
`json.Unmarshal` (`map[string]any` with `float64` values) and
returns a typed `StepVisits`.

### 5.4 Storage encoding

- On write: `json.Marshal(task.Metadata)`. If the resulting map is
  empty (`{}` or nil), write SQL `NULL` instead.
- On read: NULL → `nil` map (`task.Metadata == nil`, callers
  initialise with `map[string]any{}` if they need to mutate).
- Never `strict` decode: tolerate any keys (this is user-editable
  data after all).

---

## 6. Engine: increment + enforcement

### 6.1 Where the cap is checked

The single point of truth is **every code path that sets a task's
`current_step_id` to a step within a workflow**. There are
exactly four of them today; we add a tiny helper and call it from
each:

| Path | File / func | Trigger | Already updates `current_step_id`? |
|---|---|---|---|
| Initial creation in a workflow | `cmd/autosk/create.go` (`resolveWorkflowEntry`) | `autosk create … --workflow` / `--agent` | yes |
| Enroll an existing task | `cmd/autosk/enroll.go` (`newEnrollCmd`) | `autosk enroll …` | yes |
| Resume to a different step | `cmd/autosk/resume.go` (`newResumeCmd`) | `autosk resume <id> --to STEP` | yes |
| Workflow advance after `step next` | `internal/daemon/executor/executor.go` (`advanceTask`) | engine consumes a step_signal | yes (when target is a sibling step) |

We also need to handle the resume-to-same-step case carefully —
see §6.3.

### 6.2 The helper

In `internal/workflow` (or `internal/step`, alongside the existing
signal store — TBD during implementation):

```go
// EnterStep is the atomic "move task into step" operation. Used by
// every code path that sets a task's current_step_id to a workflow
// step. Returns ErrMaxVisitsExceeded when the next visit would cross
// the step's max_visits cap; in that case neither the task nor the
// metadata is touched.
//
// On success: increments metadata.step_visits[step_id] and updates
// tasks.current_step_id + tasks.status = 'in_workflow' inside a
// single tx.
func EnterStep(ctx context.Context, tasks store.Store, taskID, stepID string) error
```

Implementation outline:

1. Load the task row + the step row (need `max_visits`) inside the
   write lock.
2. `next := metadata.step_visits[stepID] + 1`.
3. If `step.MaxVisits > 0 && next > step.MaxVisits` → return
   `ErrMaxVisitsExceeded{Step: stepName, Visits: next-1, Max: max}`.
4. Else: write metadata back (with the bumped counter) AND set
   `current_step_id = stepID`, `status = 'in_workflow'` in one
   `UpdateTask` call. Use the new `UpdateMetadata` helper from §5.2
   to keep it atomic.

Sentinel error:

```go
// ErrMaxVisitsExceeded is returned by EnterStep when the next visit
// would cross the step's max_visits cap. Callers persist a useful
// `error` string on daemon_runs by formatting this value.
type MaxVisitsExceededError struct {
    StepID   string
    StepName string
    Visits   int // visits BEFORE the would-be increment
    Max      int
}

func (e MaxVisitsExceededError) Error() string {
    return fmt.Sprintf(
        "step_max_visits_exceeded: step %q reached visits=%d max=%d",
        e.StepName, e.Visits, e.Max,
    )
}
```

### 6.3 Wiring the helper

**`advanceTask` (`internal/daemon/executor/executor.go`).**

Today it switches on the signal kind and emits a single `UpdateTask`
patch. Refactor: when the signal carries a sibling step
(`sig.NextStepName != ""`), call `workflow.EnterStep(...)` instead
of the raw patch. The other branches (human_feedback / done /
cancelled) do NOT advance into a step — leave them as plain
`UpdateTask`.

If `EnterStep` returns `MaxVisitsExceededError`, the caller wraps it
and returns it up to `Run`, where `failTerminal` ⇒ `MarkFailed` +
`parkTaskOnFailure` does the right thing already:

- `daemon_runs.error = "step_max_visits_exceeded: …"`.
- `tasks.status = human_feedback` (parking happens because the task
  is still `in_workflow` at the moment of failure).
- `tasks.current_step_id` is **preserved** (we never moved it).

Note: when we hit the cap, we are *one step away from the
over-limit step*. The executor is finishing the run on step **A**
and the agent chose to move to step **B** (the one with the cap).
The user's intent is "park the task at A". Since the run for A
itself succeeded, `parkTaskOnFailure` parking with
`current_step_id = A` is exactly correct. After the human resets
visits, `autosk resume <id>` resumes at A and the same agent gets
a chance to pick a different transition.

**`enroll` / `create --workflow` / `resume --to`.**

Each of these does an `UpdateTask` setting `WorkflowID` +
`CurrentStepID` + `Status`. They will instead:

1. (Only for `enroll` and `create --workflow`.) Set
   `workflow_id` first (separate trivial patch) so the task is
   associated with the workflow before we touch metadata. Optional
   micro-optimisation — could also be folded into the same tx via
   an enriched helper signature `EnterStep(... , workflowID
   optional)`.
2. Call `workflow.EnterStep(ctx, store, taskID, stepID)`. On
   `MaxVisitsExceededError`, surface a user-facing CLI error:
   ```
   cannot enter step "dev": already at max_visits 5; reset with
   `autosk metadata reset-visits <id> --step dev`
   ```
   and exit non-zero, leaving the task in whatever status it was
   in (`new` for `create`/`enroll`; `human_feedback` for `resume`).

The simpler refactor — and what we'll actually ship — is:

- Add `workflowID` and an opt-in flag to `EnterStep` so the helper
  owns the *entire* set: `workflow_id`, `current_step_id`,
  `status`, and the metadata counter update, all in one tx.
- Signature becomes:
  ```go
  type EnterStepInput struct {
      TaskID      string
      StepID      string
      WorkflowID  string // optional: when set, also stamped into tasks
  }
  func EnterStep(ctx, tasks, in EnterStepInput) error
  ```
- Each call site builds an `EnterStepInput` and uses it.

This keeps the read-modify-write atomic in *every* path, which is
the real win.

### 6.4 Resume edge cases

`autosk resume <id>` with **no** `--to`:

- This is the "human typed comments, send the task back to the
  same step" case. The current code sets
  `status=in_workflow`, leaves `current_step_id` unchanged.
- Per the chosen semantics (option "on_transition_in") this is NOT
  a new transition into the step. We must therefore preserve today's
  behaviour and NOT call `EnterStep`: just do the existing simple
  `UpdateTask({Status})` patch (no `CurrentStepID` change). No
  visit increment.

`autosk resume <id> --to STEP`:

- Even when `STEP == current_step_id`, we treat it as a deliberate
  transition; `EnterStep` is called and the counter increments. This
  matches the user's mental model: "I'm sending it back into review
  for another pass."
- If that triggers `MaxVisitsExceededError`, resume errors out
  before mutating anything; the user is told to reset.

`autosk resume <id> --to STEP` when the task is in
`human_feedback` because of a previous max-visits fail:

- Without resetting the counter first, this will fail again. That's
  the intended behaviour — the user has to decide whether to
  bump/reset and then resume. The CLI error includes the
  reset-visits hint.

### 6.5 Concurrency

- Only one `daemon_runs` row per task is `queued|running` at a time
  (existing `NOT EXISTS` in the poller). So `advanceTask` can never
  race itself on the same task.
- `EnterStep` runs under doltlite's single-writer lock, which
  serializes any concurrent CLI verb (enroll, resume, create,
  metadata-set) against the daemon's writes.
- Two simultaneous `metadata set` calls serialize trivially.

### 6.6 What happens if the limit fires inside `advanceTask`?

Concrete sequence on a typical bounce loop:

1. Task is on step `dev`, run #N spawns the developer agent.
2. Developer finishes, calls `autosk step next $id --to review`.
3. Executor wakes up post-turn, reads `step_signals`, calls
   `advanceTask(... sig{NextStepName:"review"})`.
4. `advanceTask` resolves `review`'s step row, calls
   `EnterStep(... review.ID)`. Visit count for `review` is already
   at `max_visits` (we're trying to push to N+1).
5. `EnterStep` returns `MaxVisitsExceededError`. `advanceTask`
   returns it; `Run` returns it; the deferred path runs
   `failTerminal` which calls `MarkFailed("step_max_visits_exceeded:
   step \"review\" reached visits=5 max=5")` and
   `parkTaskOnFailure(...)`.
6. `parkTaskOnFailure` reads the task: status is still
   `in_workflow` (we never moved it past `dev`), so it patches
   `status=human_feedback`. `current_step_id` stays `dev`.
7. Daemon stops touching the task. Human runs `autosk show $id` and
   sees status=human_feedback, current_step=dev. `autosk daemon
   list` (or lazy) shows the failed run with
   `error=step_max_visits_exceeded:…`.

Human now has three options:

- `autosk metadata reset-visits $id --step review` then `autosk
  resume $id` (or `--to review` to force the re-entry now).
- `autosk metadata reset-visits $id` (all counters) then resume.
- `autosk cancel $id` to give up.

---

## 7. CLI: `autosk metadata` and `autosk show`

### 7.1 `autosk metadata show <id>`

Prints the task's full `metadata` as pretty JSON to stdout. Honors
`--json` (alias for the default behaviour: stable, formatted JSON).
With `--quiet`: prints nothing on success and exits 0.

Example:

```
$ autosk metadata show as-bea9
{
  "step_visits": {
    "st-abcd": 3,
    "st-efgh": 5
  },
  "tags": ["urgent"]
}
```

Also: a "pretty" mode `autosk metadata show <id> --visits-pretty`
that joins step ids to step names + max:

```
step_visits:
  dev       3 / 5
  review    5 / 5   (over limit!)
other keys:
  tags = ["urgent"]
```

(The "pretty" mode is best-effort: it requires the task to have a
`workflow_id` so we can resolve step ids → names. When that lookup
fails, fall back to ids.)

### 7.2 `autosk metadata set <id> --key K --value V` (and `--json-value`)

- `--key` is a dotted JSON path: `step_visits.st-abcd`, `tags`,
  `foo.bar.baz`. Limited to plain object navigation; no array
  indexing (out of scope).
- `--value` is parsed as a JSON literal when it starts with
  `[ { " t f n - 0..9`, otherwise treated as a string. Practical
  examples:
  - `--key tags --value '["urgent","p0"]'` → array
  - `--key step_visits.st-abcd --value 0` → integer
  - `--key notes --value "needs design review"` → string
- `--json-value FILE` reads the JSON value from a file (or `-` for
  stdin). Mutually exclusive with `--value`.
- Refuses to set `step_visits` to a non-object or a non-int leaf
  (validates against the typed accessor; bad input → exit 1 with a
  clear message). This guards humans from corrupting the engine's
  view.
- On success, prints the updated metadata (unless `--quiet`).

### 7.3 `autosk metadata unset <id> --key K`

Deletes the key (and any parent objects that become empty).
No-op if the key doesn't exist (exit 0 with a "(no change)"
message unless `--quiet`).

### 7.4 `autosk metadata reset-visits <id> [--step NAME|--step-id ID]`

The "escape hatch" verb for the limit feature. Behaviour:

- No flag: clears `metadata.step_visits` entirely (removes the
  key).
- `--step NAME`: requires the task to have `workflow_id`; resolves
  `(workflow_id, NAME)` → step_id and deletes only that entry.
- `--step-id ID`: deletes only that entry (no name resolution; useful
  for orphaned counters).

Always commits with the standard `commitWrite(ctx, s, ...)` call so
the dolt audit log is informative ("reset-visits as-bea9 --step
review").

### 7.5 `autosk show <id>` rendering

When `metadata.step_visits` is non-empty, the human renderer (not
JSON) gains a single-line summary:

```
visits: dev 3/5, review 5/5*, validator 1
```

- `*` marks any step at or over its cap.
- Steps with no cap render as bare `N` (no denominator).
- Step ids that don't resolve to a step in the task's workflow are
  shown as `<unknown:st-…> N`.

JSON output: `show --json` already prints the task struct via
`render.TaskJSONTo`. Extend the JSON output to include the full
`metadata` object verbatim (under a `metadata` key). Numeric values
inside `step_visits` round-trip as JSON integers.

### 7.6 Commit messages

Every `metadata` mutating verb commits with a message that includes
the verb and the task id, mirroring existing `commitWrite(...)`
calls:

```
metadata set as-bea9 --key tags
metadata unset as-bea9 --key notes.tmp
metadata reset-visits as-bea9 --step review
```

---

## 8. Agent surface (no changes for v1)

The agent does NOT learn about visit counts or `max_visits`. The
plan deliberately keeps the cap invisible to the agent so that:

- The same prompt template ships regardless of step config.
- Buggy agents can't try to "game" the counter (e.g. terminate
  early because they sense the cap).
- The feature is purely a human-side guardrail.

Future enhancement (out of scope): surface "visit N of M" in the
prompt for steps that have a cap, to help long-context agents
self-terminate. Park as a follow-up plan.

`autosk step next <id> --to <sibling>` does NOT pre-check the cap.
The cap check happens inside `advanceTask` after the signal lands.
This means a "wasted" `step next` call from an agent will fail the
run, but consistently — we don't want two places enforcing the
same rule.

---

## 9. Tests

### 9.1 Unit

- `internal/workflow/parse_test.go`:
  - `max_visits` round-trips through ParseFile/ParseReader.
  - Negative value rejected at Validate.
  - Absence ≡ zero.
  - Round-trip through `Store.Create` + reload preserves the value.
- `internal/workflow/store_test.go`:
  - `scanStep` returns `max_visits` correctly on every code path
    that touches `steps`.
  - `EnsureSingle` produces synthetic step with `max_visits=0`.
- New `internal/workflow/enterstep_test.go` (or wherever the helper
  lands):
  - Happy path: counter bumps from 0→1, task updated.
  - Cap of 5, current=5 → returns `MaxVisitsExceededError`, no DB
    mutation.
  - Cap of 0 (unlimited) → counter bumps indefinitely, no error.
  - Concurrent callers serialise correctly (two parallel goroutines,
    cap of 1: exactly one wins).
- `internal/meta` (or wherever the typed accessor lives):
  - JSON round-trip of `step_visits` survives the
    `float64`-after-`json.Unmarshal` issue.
  - `MutateStepVisits` removes the key when the map becomes empty.

### 9.2 Executor

`internal/daemon/executor/executor_test.go`:

- Sibling-step transition: a successful `advanceTask` to a step with
  `max_visits=3` increments visit count from 0 to 1, the
  `daemon_runs` row marks done.
- Cap fires: pre-populate metadata `{step_visits: {st-review: 5}}`,
  drive a run that signals `--to review`, assert:
  - `daemon_runs.status='failed'`,
  - `daemon_runs.error` starts with `step_max_visits_exceeded:`,
  - `tasks.status='human_feedback'`,
  - `tasks.current_step_id` is the OLD step (the one we ran on),
  - `tasks.metadata.step_visits.st-review` is still 5 (NOT
    incremented).
- Cancellation race: cap fires concurrently with operator hitting
  `done` — the `parkTaskOnFailure` skip-on-non-in-workflow guard
  still applies; the test asserts no clobber.

### 9.3 CLI / golden

- `cmd/autosk/enroll_test.go`: enroll into a workflow where
  first_step has `max_visits=1`. First enroll: counter is `1`. Try
  to re-enroll a `new` task (impossible by design, but test the
  resume path): `resume --to first_step` from `human_feedback`
  surfaces the cap error.
- `cmd/autosk/create_test.go` (or whichever exists): `create
  --workflow X` populates `metadata.step_visits[<first_step>] = 1`.
- New `cmd/autosk/metadata_test.go`:
  - `metadata show` of a task without metadata prints `{}`.
  - `metadata set --key foo --value bar` round-trips.
  - `metadata set --key step_visits.st-x --value "not-an-int"`
    fails with a validation error.
  - `metadata unset` removes a key and a now-empty parent.
  - `metadata reset-visits --step review` resolves the name and
    deletes only that entry.
  - `metadata reset-visits` with no flags clears the whole map.

### 9.4 End-to-end

Single new e2e in `cmd/autosk/workflow_e2e_test.go`:

- Workflow `dev → review` with both steps `max_visits: 2` and
  transitions `dev→review` (always) and `review→dev` (always — to
  force a loop).
- Drive 4 advances; on the 5th, assert that the task ends up
  `human_feedback`, the run errored with
  `step_max_visits_exceeded`, and `metadata.step_visits` shows
  exactly `{st-dev: 2, st-review: 2}`.
- Then `autosk metadata reset-visits <id>` + `autosk resume
  <id>` continues the loop (assert two more advances are accepted
  before the cap fires again).

---

## 10. Out of scope / follow-ups

Tracked as TODOs, not blockers for this plan:

1. **Lazy TUI integration.** The inspector should expose a
   "Metadata" panel (read-only by default, with a `M`-keybind to
   open `$EDITOR` on the JSON). Plan in a separate file once the
   CLI lands; the lazy TUI is mid-rewrite (see
   `20260520-Lazy-Impl-Plan.md`).
2. **`metadata edit <id>`.** A `$EDITOR`-based wrapper around
   `metadata show` + `metadata set` for free-form edits. Easy
   follow-up; not needed for v1 since set/unset/reset-visits cover
   the planned use cases.
3. **`workflow.default_max_visits`.** Per-workflow default so
   typical "dev↔review" loops don't repeat the cap on every step.
4. **Visible counter in agent prompt.** Adds "you are on step `dev`
   (visit 3/5)" to the rendered prompt. Has UX risk — see §8 —
   prove it useful first.
5. **Per-step `max_visits` exposed in `autosk workflow show`.**
   The renderer should display the cap alongside agent name; tiny
   change but separate concern.

---

## 11. Implementation order (rough)

Suggested phases; each can land on its own PR and be tested
independently.

| Phase | Work | Tests added |
|---|---|---|
| P1 | Migration 004; `tasks.metadata` plumbing in `store.Task` / `TaskPatch` / `doltlite`; `UpdateMetadata` helper; `internal/meta` accessor. | store + meta unit tests; round-trip tests through `CreateTask/UpdateTask`. |
| P2 | `max_visits` in `internal/workflow` (parser, validate, store, scan, Step struct). Synthetic workflow stays uncapped. | workflow parse/store unit tests; `EnsureSingle` test. |
| P3 | `workflow.EnterStep` helper + `MaxVisitsExceededError`. | helper unit tests (incl. concurrency). |
| P4 | Wire `EnterStep` into `advanceTask`, `enroll`, `create --workflow`, `resume --to`. Adjust error messages. | executor cap tests; CLI enroll/create/resume tests. |
| P5 | `autosk metadata {show,set,unset,reset-visits}` CLI. Extend `autosk show` rendering (human + JSON). | metadata CLI tests; golden updates for `show`. |
| P6 | Docs: `docs/workflows.md` section, `docs/notes/workflow-example.json` example, `README.md` blurb. | none (docs). |
| P7 | End-to-end loop test. | one new e2e in `workflow_e2e_test.go`. |

---

## 12. Risks and caveats

- **Orphaned counters.** If a workflow is dropped + re-created with
  the same name, its step ids change. Existing tasks pointing at the
  old `workflow_id` (already broken in the v0.2 schema!) carry stale
  `step_visits` entries that nothing reads. This is harmless; the
  pretty renderer marks them as `<unknown:st-…>`. We leave them in
  place rather than auto-garbage-collecting; the user can clear
  them with `reset-visits` if they care.
- **Metadata as a footgun.** Free-form `tasks.metadata` will tempt
  agents and CLI plugins to dump arbitrary blobs in there. We don't
  yet have a "registered namespaces" mechanism. For now, document
  loudly that `step_visits` is reserved and the engine refuses
  malformed shapes; defer namespace enforcement to a future plan.
- **Failure obscures cause.** The user only sees the cap fail via
  `daemon_runs.error` and the task being parked. If they don't
  check daemon list or lazy, they might just `resume` and watch it
  fail again. Mitigation: `autosk show` will surface the visits
  summary (`5/5*`) inline, which is the most likely place they'll
  look. Optionally we could also add a synthetic comment, but the
  user opted against that (option `fail_park_keep_step` over
  `fail_with_comment`); revisit if humans miss the cause in
  practice.
- **Resume-to-same-step semantics.** Documenting the
  "`resume` without `--to` does NOT count, `--to current_step`
  DOES count" rule clearly is essential — easy to get confused
  about. The `autosk resume --help` text should say this explicitly.
