package tui

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/jesseduffield/gocui"

	"autosk/internal/daemon/runstore"
	"autosk/internal/lazy/datasource"
	"autosk/internal/store"
)

// refreshResult is the snapshot computed by fetchRefresh and applied
// by applyRefreshLocked. Splitting compute from apply lets unit
// tests drive the cache-preservation / fallback-counter invariants
// without spinning up a real gocui.Gui.
type refreshResult struct {
	tasks      []datasource.Task
	taskSearch string // post-facet free text remainder
	terr       error
	sessions   []datasource.Session // renamed from jobs
	serr       error                // renamed from jerr
	// taskSessionIdx mirrors the state field of the same name: a
	// per-task session-presence projection built from the
	// TaskID-UNFILTERED sessions read so the Tasks-panel ">" marker
	// doesn't disappear for unrelated tasks when scope.TaskID is
	// active. See state.taskSessionIdx for the rationale.
	taskSessionIdx   taskSessionIndex // renamed from taskJobIdx
	visibleWorkflows []datasource.Workflow
	workflowSearch   string
	werr             error
	agents           []datasource.Agent
	agentSearch      string
	aerr             error
	health           datasource.Health
	sessionsSearch   string // renamed from jobsSearch

	selectedTaskID   string
	selectedComments []datasource.Comment
	hasCommentsErr   bool
	// NOTE: selectedSignals removed in v2 - no signals

	fallbacksNow uint64
}

// refreshAll re-fetches every panel from the datasource, applies the
// active scope/filter chips, and posts the result back to the UI
// thread via g.Update.
//
// Called on the daemon's task-changed/project-changed push (the steady-state
// driver), the long safety re-sync tick, AND on demand (R key, after a write).
func (gu *Gui) refreshAll() {
	timeout := 5 * gu.opts.Refresh
	if timeout < 500*time.Millisecond {
		timeout = 500 * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(gu.ctx, timeout)
	defer cancel()

	r := gu.fetchRefresh(ctx)
	gu.g.Update(func(_ *gocui.Gui) error {
		gu.applyRefreshLocked(r)
		return nil
	})
}

// fetchRefresh is the pure-data half of refreshAll: it reads from
// the datasource and returns a snapshot. Safe to call from tests
// (no gocui dependency). The caller is expected to hand the result
// to applyRefreshLocked, typically inside a g.Update closure.
//
// The wall-clock duration of the read burst is recorded in
// gu.lastFetchNS at the end. adaptiveTickLoop reads that field to
// throttle itself when the datasource gets slow — see the comment
// on adaptiveTickLoop for the failure mode this defends against.
func (gu *Gui) fetchRefresh(ctx context.Context) refreshResult {
	fetchStart := time.Now()
	defer func() {
		gu.lastFetchNS.Store(int64(time.Since(fetchStart)))
	}()
	// Snapshot scope + filters so we don't hold the lock across IO.
	var sc scope
	var ft filterState
	gu.st.withRLock(func() {
		sc = gu.st.scope
		ft = gu.st.filter
	})

	// Workflows are read-only from code in v2, no wfByName resolver needed
	workflows, werr := gu.ds.Workflows(ctx)

	// The dashboard surfaces ALL statuses by default — a non-nil but
	// empty Statuses slice is the datasource's escape hatch for
	// "no status WHERE clause" (nil would mean "OpenStatuses", i.e.
	// what `autosk list` defaults to). The operator needs to see
	// recently-done / cancel work alongside the open queue,
	// and the by-recency sort below keeps stale terminal rows from
	// drowning out fresh work.
	tasksFilter := datasource.TaskFilter{Statuses: []store.Status{}}
	if sc.WorkflowName != "" {
		tasksFilter.WorkflowName = sc.WorkflowName // renamed from WorkflowID
	}
	// NOTE: Agent scoping removed in v2
	applyFacetFilter(&tasksFilter, ft.Tasks)
	tasks, terr := gu.ds.Tasks(ctx, tasksFilter)
	if terr != nil {
		dlog("refresh: ds.Tasks: %v", terr)
	}

	// Sessions fetch intentionally OMITS sc.TaskID from the server-side
	// filter — we need the full set so the Tasks-panel marker column
	// (taskSessionIdx, below) can mark every task with at least one session,
	// not only the one currently scoped. The TaskID filter is then
	// re-applied client-side for the Sessions-panel slice.
	allSessions, serr := gu.ds.Sessions(ctx, "") // empty taskID = all sessions
	if serr != nil {
		dlog("refresh: ds.Sessions: %v", serr)
	}
	taskSessionIdx := taskSessionIndexFromSessions(allSessions)
	sessions := allSessions
	if sc.TaskID != "" {
		// Task scope is the narrowest — a task belongs to exactly one
		// workflow, so filtering by TaskID already constrains the
		// workflow.
		sessions = filterSessionsByTaskID(allSessions, sc.TaskID)
	} else if sc.WorkflowName != "" {
		// Workflow-only scope: session.list has no server-side workflow
		// filter, but SessionMeta carries .Workflow, so scope the panel
		// slice client-side to the active workflow (parity with v1's
		// workflow-scoped jobs view). The marker column above stays
		// derived from allSessions, so every task still shows its
		// has-sessions glyph regardless of the active scope.
		sessions = filterSessionsByWorkflow(allSessions, sc.WorkflowName)
	}

	// Workflows are now read-only from code (v2), no synthetic filtering needed
	visibleWorkflows := workflows
	agents, aerr := gu.ds.Agents(ctx)
	health, _ := gu.ds.Healthz(ctx)

	var selectedTaskID string
	gu.st.withRLock(func() {
		if t, ok := gu.st.selectedTask(); ok {
			selectedTaskID = t.ID
		}
	})
	var selectedComments []datasource.Comment
	hasCommentsErr := false
	if selectedTaskID != "" {
		var cerr error
		selectedComments, cerr = gu.ds.Comments(ctx, selectedTaskID)
		if cerr != nil {
			dlog("refresh: ds.Comments(%s): %v", selectedTaskID, cerr)
			// Intentionally do NOT overwrite an existing cache entry on
			// error: stale-but-shown is better than empty-and-flickering.
			// The hasCommentsErr flag guards the write in applyRefreshLocked.
			hasCommentsErr = true
			selectedComments = nil
		}
		// NOTE: signals removed in v2
	}

	// Pick up the live datasource's fallback counter so renderStatusBar
	// can surface a 'flaky' chip when reads silently switched to the
	// offline base. The interface is optional: pure offline mode has
	// nothing to count.
	var fallbacksNow uint64
	if f, ok := gu.ds.(interface{ Fallbacks() uint64 }); ok {
		fallbacksNow = f.Fallbacks()
	}

	return refreshResult{
		tasks: tasks, taskSearch: tasksFilter.Search, terr: terr,
		sessions: sessions, sessionsSearch: ft.Sessions, serr: serr, // renamed from jobs
		taskSessionIdx:   taskSessionIdx, // renamed from taskJobIdx
		visibleWorkflows: visibleWorkflows, workflowSearch: ft.Workflows, werr: werr,
		agents: agents, agentSearch: ft.Agents, aerr: aerr,
		health:           health,
		selectedTaskID:   selectedTaskID,
		selectedComments: selectedComments, hasCommentsErr: hasCommentsErr,
		// NOTE: selectedSignals removed in v2
		fallbacksNow: fallbacksNow,
	}
}

// applyRefreshLocked mutates the model from a refreshResult. Must be
// called inside a g.Update closure (which is the only place gocui
// allows touching view state). Held under st.mu.Lock.
//
// Detail-pane side effects:
//
//   - When the currently-selected session transitions from running to
//     terminal between snapshots, its transcript entry's loadedAt
//     is zeroed so the next selection-driven hydration refetches
//     the archive (so final-flushed events appear). The schedule
//     is queued AFTER the lock is released, via OnWorker, so we
//     don't block the apply path on the refetch.
//   - When state.sessionLiveSessionID no longer appears in the sessions
//     slice (filter / scope removed it), stopSessionLive tears down
//     the subscription.
//
// Note: this path INTENTIONALLY does NOT call clearSessionInputIfStale.
// The selected session's sessionID can shift between snapshots without
// any operator action (e.g. a new session inserted at index 0 shoves
// the previously-cursored row down to index 1 while sessionCursor
// stays pinned to 0). Wiping the draft on that shift would silently
// discard mid-typing text the operator never asked us to touch.
// The anti-leak guarantee is enforced on the operator-driven
// explicit cursor move via afterCursorMove(panelSessions), which is
// the only authored event we want anchoring the clear.
func (gu *Gui) applyRefreshLocked(r refreshResult) {
	var (
		refetchSessionID string
		stopLive         bool
	)
	gu.st.withLock(func() {
		if r.terr == nil {
			oldTaskID := ""
			if t, ok := gu.st.selectedTask(); ok {
				oldTaskID = t.ID
			}
			gu.st.tasks = sortTasksByRecency(applyTaskSearch(r.tasks, r.taskSearch))
			if idx, ok := findIndexByKey(gu.st.tasks, oldTaskID, func(t datasource.Task) string { return t.ID }); ok {
				gu.st.taskCursor = idx
			} else {
				gu.st.taskCursor = clampCursor(gu.st.taskCursor, len(gu.st.tasks))
			}
		}
		if r.serr == nil {
			// Detect transitions for the currently-selected session
			// BEFORE we swap the sessions slice. Two effects:
			//
			//   running→terminal (same session, status moved): zero the
			//     transcript loadedAt so the next archive load picks
			//     up final-flushed events.
			//   focused == panelSessionInput, post-swap selected session
			//     not live: revert focus to panelSessions so the
			//     layout-pass SetCurrentView lands on a real view. This
			//     branch fires whether the SAME session went terminal
			//     under the operator, OR the cursor's sessionID drifted
			//     to a different (terminal) session because the sessions
			//     slice reshuffled (e.g. a new queued session lands at
			//     index 0, pushing the previously-cursored row down).
			//     Both paths would otherwise leave the caret in
			//     phantom-focus on a deleted view.
			prevSession, hadPrev := gu.st.selectedSession()
			oldSessionID := ""
			if hadPrev {
				oldSessionID = prevSession.ID
			}
			// The daemon returns sessions newest-first (by id, descending), so the
			// panel renders them top-to-bottom as-is.
			gu.st.sessions = applySessionSearch(r.sessions, r.sessionsSearch)
			if idx, ok := findIndexByKey(gu.st.sessions, oldSessionID, func(s datasource.Session) string { return s.ID }); ok {
				gu.st.sessionCursor = idx
			} else {
				gu.st.sessionCursor = clampCursor(gu.st.sessionCursor, len(gu.st.sessions))
			}
			gu.st.taskSessionIdx = r.taskSessionIdx
			if cur, ok := gu.st.selectedSession(); ok {
				if hadPrev && cur.ID == prevSession.ID {
					wasRunning := runstore.RunStatus(prevSession.Status) == runstore.StatusRunning
					isRunning := runstore.RunStatus(cur.Status) == runstore.StatusRunning
					if wasRunning && !isRunning {
						if te, ok := gu.st.sessionTranscript[cur.ID]; ok {
							te.loadedAt = time.Time{}
						}
						refetchSessionID = cur.ID
					}
				}
				// General phantom-focus guard: any time the operator
				// is in the input but the (post-swap) selected session
				// is no longer live, revert to panelSessions. Covers
				// both same-session running→terminal AND reshuffle to
				// a different non-live session.
				if gu.st.focused == panelSessionInput && !isSessionLive(cur) {
					gu.st.focused = panelSessions
				}
			} else if gu.st.focused == panelSessionInput {
				// Sessions slice emptied / filtered out entirely while
				// the operator was in the input. Same phantom-focus
				// risk — the input view will be deleted on the next
				// layout pass.
				gu.st.focused = panelSessions
			}
			// If the streamed session vanished from the slice (e.g. a
			// filter just removed it), tear down the subscription so
			// we're not holding an open live tail for a session the operator
			// can't see.
			if gu.st.sessionLiveSessionID != "" {
				found := false
				for i := range gu.st.sessions {
					if gu.st.sessions[i].ID == gu.st.sessionLiveSessionID {
						found = true
						break
					}
				}
				if !found {
					stopLive = true
				}
			}
		}
		if r.werr == nil {
			oldWfName := "" // renamed
			if w, ok := gu.st.selectedWorkflow(); ok {
				oldWfName = w.Name // v2 workflows use Name as key, not ID
			}
			gu.st.workflows = applyWfSearch(r.visibleWorkflows, r.workflowSearch)
			if idx, ok := findIndexByKey(gu.st.workflows, oldWfName, func(w datasource.Workflow) string { return w.Name }); ok {
				gu.st.workflowCursor = idx
			} else {
				gu.st.workflowCursor = clampCursor(gu.st.workflowCursor, len(gu.st.workflows))
			}
		}
		if r.aerr == nil {
			oldAgentName := ""
			if a, ok := gu.st.selectedAgent(); ok {
				oldAgentName = a.Name
			}
			gu.st.agents = applyAgentSearch(r.agents, r.agentSearch)
			if idx, ok := findIndexByKey(gu.st.agents, oldAgentName, func(a datasource.Agent) string { return a.Name }); ok {
				gu.st.agentCursor = idx
			} else {
				gu.st.agentCursor = clampCursor(gu.st.agentCursor, len(gu.st.agents))
			}
		}
		gu.st.health = r.health
		gu.st.fallbacksLast = gu.st.fallbacksNow
		gu.st.fallbacksNow = r.fallbacksNow
		if r.selectedTaskID != "" && !r.hasCommentsErr && r.selectedComments != nil {
			evictCacheIfNeeded(gu.st.comments, r.selectedTaskID, commentsCacheMax)
			gu.st.comments[r.selectedTaskID] = r.selectedComments
		}
		// NOTE: signals cache removed in v2
	})
	if stopLive {
		gu.stopSessionLive() // renamed from stopJobLive
	}
	if refetchSessionID != "" && gu.g != nil {
		sessionID := refetchSessionID
		gu.g.OnWorker(func(_ gocui.Task) error {
			gu.loadSessionArchive(sessionID) // renamed from loadJobArchive
			return nil
		})
	}
}

// evictCacheIfNeeded drops one arbitrary non-key entry from m if the
// map is at maxLen AND key isn't already present. (When key is
// already in the map we're replacing it, not growing, so no eviction
// is needed.) Generic over the cache's value type so the comments
// and signals caches share the policy.
//
// The parameter is named maxLen (not cap) to avoid shadowing the Go
// built-in cap — a small landmine if anyone ever needs cap(slice)
// inside this function in the future.
// findIndexByKey searches slice for the first element whose extracted key
// matches key.  Returns (index, true) on a hit, (0, false) when the slice
// is empty or no element matches.
func findIndexByKey[T any](slice []T, key string, keyFn func(T) string) (int, bool) {
	for i, v := range slice {
		if keyFn(v) == key {
			return i, true
		}
	}
	return 0, false
}

func evictCacheIfNeeded[V any](m map[string]V, key string, maxLen int) {
	if _, exists := m[key]; exists {
		return
	}
	if len(m) < maxLen {
		return
	}
	for k := range m {
		if k == key {
			continue
		}
		delete(m, k)
		return
	}
}

func clampCursor(cur, n int) int {
	if n == 0 {
		return 0
	}
	if cur < 0 {
		return 0
	}
	if cur >= n {
		return n - 1
	}
	return cur
}

// applyFacetFilter parses simple `key:value` chips from a filter
// string and writes them into the task filter. Anything that doesn't
// match a known facet is left in the trailing free-text search.
//
// Supported facets (v2):
//
//	status:<status>  task status
//	wf:<name>        workflow name (matched directly)
//
// Free text is matched as a substring against id + title.
func applyFacetFilter(f *datasource.TaskFilter, expr string) {
	tokens := strings.Fields(expr)
	var free []string
	for _, tok := range tokens {
		if !strings.Contains(tok, ":") {
			free = append(free, tok)
			continue
		}
		k, v, _ := strings.Cut(tok, ":")
		switch strings.ToLower(k) {
		case "status":
			f.Statuses = []store.Status{store.Status(v)}
		case "wf":
			// v2 uses workflow names directly
			f.WorkflowName = v
		// v2 dropped the priority + agent facets (no priority field; agents
		// are code, no per-task author).
		default:
			free = append(free, tok)
		}
	}
	f.Search = strings.Join(free, " ")
}

// Allocate the output slice rather than aliasing into `in` with
// `in[:0]`: the aliasing trick is correct only if the caller's `in`
// is owned by us. Datasource returns a fresh slice today, but a
// future caller may pass a shared slice; defensive copy avoids the
// footgun. The allocation is cheap (the slices are small — dashboard
// panels show ≲ 200 rows).

// sortTasksByRecency orders tasks newest-first by their last activity
// timestamp. UpdatedAt wins when set (the row was touched after
// creation); otherwise CreatedAt is used as the fallback so brand-new
// rows still sort sensibly against older edits. Stable sort keeps
// rows with identical timestamps in the datasource's emit order —
// the offline ListTasks already breaks ties by created_at ASC, which
// becomes a deterministic tie-break here.
func sortTasksByRecency(in []datasource.Task) []datasource.Task {
	if len(in) < 2 {
		return in
	}
	out := make([]datasource.Task, len(in))
	copy(out, in)
	sort.SliceStable(out, func(i, j int) bool {
		return taskRecency(out[i]).After(taskRecency(out[j]))
	})
	return out
}

// taskRecency picks the freshest timestamp on a task: UpdatedAt
// when present, falling back to CreatedAt. Pulled out as a named
// helper so the sort comparator stays one line and the policy is
// testable in isolation if we ever need to grow it (e.g. factor in
// the latest comment / signal time).
func taskRecency(t datasource.Task) time.Time {
	if !t.UpdatedAt.IsZero() {
		return t.UpdatedAt
	}
	return t.CreatedAt
}

func applyTaskSearch(in []datasource.Task, expr string) []datasource.Task {
	needle := strings.ToLower(strings.TrimSpace(expr))
	if needle == "" {
		return in
	}
	out := make([]datasource.Task, 0, len(in))
	for _, t := range in {
		if strings.Contains(strings.ToLower(t.ID), needle) ||
			strings.Contains(strings.ToLower(t.Title), needle) {
			out = append(out, t)
		}
	}
	return out
}

// filterSessionsByTaskID returns the subset of sessions whose TaskID matches
// id. Used by fetchRefresh to apply scope.TaskID client-side after
// the server-side fetch dropped that filter (so the Tasks-panel
// marker index can see ALL tasks' sessions, not just the scoped one's).
// Allocates a fresh slice so the caller's input isn't aliased.
func filterSessionsByTaskID(in []datasource.Session, id string) []datasource.Session {
	if id == "" {
		return in
	}
	out := make([]datasource.Session, 0, len(in))
	for i := range in {
		if in[i].TaskID == id {
			out = append(out, in[i])
		}
	}
	return out
}

// filterSessionsByWorkflow keeps only sessions whose .Workflow matches
// wf. Used to scope the Sessions panel under a workflow-only scope
// (session.list has no server-side workflow filter). An empty wf
// returns the input unchanged.
func filterSessionsByWorkflow(in []datasource.Session, wf string) []datasource.Session {
	if wf == "" {
		return in
	}
	out := make([]datasource.Session, 0, len(in))
	for i := range in {
		if in[i].Workflow == wf {
			out = append(out, in[i])
		}
	}
	return out
}

func applySessionSearch(in []datasource.Session, expr string) []datasource.Session {
	needle := strings.ToLower(strings.TrimSpace(expr))
	if needle == "" {
		return in
	}
	out := make([]datasource.Session, 0, len(in))
	for _, s := range in {
		hay := strings.ToLower(s.ID + " " + string(s.Status) + " " + s.Workflow + " " + s.Step)
		if strings.Contains(hay, needle) {
			out = append(out, s)
		}
	}
	return out
}

func applyWfSearch(in []datasource.Workflow, expr string) []datasource.Workflow {
	needle := strings.ToLower(strings.TrimSpace(expr))
	if needle == "" {
		return in
	}
	out := make([]datasource.Workflow, 0, len(in))
	for _, w := range in {
		if strings.Contains(strings.ToLower(w.Name), needle) {
			out = append(out, w)
		}
	}
	return out
}

func applyAgentSearch(in []datasource.Agent, expr string) []datasource.Agent {
	needle := strings.ToLower(strings.TrimSpace(expr))
	if needle == "" {
		return in
	}
	out := make([]datasource.Agent, 0, len(in))
	for _, a := range in {
		if strings.Contains(strings.ToLower(a.Name), needle) {
			out = append(out, a)
		}
	}
	return out
}
