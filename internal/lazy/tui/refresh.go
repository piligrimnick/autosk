package tui

import (
	"context"
	"fmt"
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
	jobs       []datasource.Job
	jerr       error
	// taskJobIdx mirrors the state field of the same name: a
	// per-task job-presence projection built from the
	// TaskID-UNFILTERED jobs read so the Tasks-panel ">" marker
	// doesn't disappear for unrelated tasks when scope.TaskID is
	// active. See state.taskJobIdx for the rationale.
	taskJobIdx       taskJobIndex
	visibleWorkflows []datasource.Workflow
	workflowSearch   string
	werr             error
	agents           []datasource.Agent
	agentSearch      string
	aerr             error
	health           datasource.Health
	jobsSearch       string

	selectedTaskID   string
	selectedComments []datasource.Comment
	hasCommentsErr   bool
	selectedSignals  []datasource.Signal
	hasSignalsErr    bool

	fallbacksNow uint64
}

// refreshAll re-fetches every panel from the datasource, applies the
// active scope/filter chips, and posts the result back to the UI
// thread via g.Update.
//
// Called on the 2s tick AND on demand (R key, after a write).
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

	// Workflows are needed both as a list and as a wf-name→wf-id
	// resolver for the wf:<name> facet on the Tasks filter (which the
	// help text in openFilter advertises). Fetch them first so the
	// facet parser can resolve names before we hit Tasks.
	workflows, werr := gu.ds.Workflows(ctx, true)
	wfByName := map[string]string{}
	if werr == nil {
		for _, w := range workflows {
			wfByName[strings.ToLower(w.Name)] = w.ID
		}
	}

	// The dashboard surfaces ALL statuses by default — a non-nil but
	// empty Statuses slice is the datasource's escape hatch for
	// "no status WHERE clause" (nil would mean "OpenStatuses", i.e.
	// what `autosk list` defaults to). The operator needs to see
	// recently-done / cancel work alongside the open queue,
	// and the by-recency sort below keeps stale terminal rows from
	// drowning out fresh work.
	tasksFilter := datasource.TaskFilter{Statuses: []store.Status{}}
	if sc.WorkflowID != "" {
		tasksFilter.WorkflowID = sc.WorkflowID
	}
	if sc.Agent != "" {
		switch sc.AgentRel {
		case agentRelAuthor:
			tasksFilter.AuthorName = sc.Agent
		case agentRelStep:
			tasksFilter.StepAgentName = sc.Agent
		}
	}
	applyFacetFilter(&tasksFilter, ft.Tasks, wfByName)
	tasks, terr := gu.ds.Tasks(ctx, tasksFilter)
	if terr != nil {
		dlog("refresh: ds.Tasks: %v", terr)
	}

	// Jobs fetch intentionally OMITS sc.TaskID from the server-side
	// filter — we need the full set so the Tasks-panel marker column
	// (taskJobIdx, below) can mark every task with at least one job,
	// not only the one currently scoped. The TaskID filter is then
	// re-applied client-side for the Jobs-panel slice. Workflow
	// scope still rides the wire because it's symmetric with the
	// Tasks panel — we don't want to mark tasks outside the scoped
	// workflow either.
	jobsFilter := datasource.JobFilter{}
	if sc.WorkflowID != "" {
		jobsFilter.WorkflowID = sc.WorkflowID
	}
	allJobs, jerr := gu.ds.Jobs(ctx, jobsFilter)
	if jerr != nil {
		dlog("refresh: ds.Jobs: %v", jerr)
	}
	taskJobIdx := taskJobIndexFromJobs(allJobs)
	jobs := allJobs
	if sc.TaskID != "" {
		jobs = filterJobsByTaskID(allJobs, sc.TaskID)
	}

	// Workflows fetched above. Re-filter to drop synthetic ones from
	// the dashboard list (we wanted them included for the facet
	// resolver, but the user-facing Workflows panel hides them).
	visibleWorkflows := make([]datasource.Workflow, 0, len(workflows))
	for _, w := range workflows {
		if w.IsSynthetic {
			continue
		}
		visibleWorkflows = append(visibleWorkflows, w)
	}
	agents, aerr := gu.ds.Agents(ctx)
	health, _ := gu.ds.Healthz(ctx)

	var selectedTaskID string
	gu.st.withRLock(func() {
		if t, ok := gu.st.selectedTask(); ok {
			selectedTaskID = t.ID
		}
	})
	var selectedComments []datasource.Comment
	var selectedSignals []datasource.Signal
	hasCommentsErr := false
	hasSignalsErr := false
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
		var serr error
		selectedSignals, serr = gu.ds.SignalsForTask(ctx, selectedTaskID)
		if serr != nil {
			dlog("refresh: ds.SignalsForTask(%s): %v", selectedTaskID, serr)
			hasSignalsErr = true
			selectedSignals = nil
		}
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
		jobs: jobs, jobsSearch: ft.Jobs, jerr: jerr,
		taskJobIdx:       taskJobIdx,
		visibleWorkflows: visibleWorkflows, workflowSearch: ft.Workflows, werr: werr,
		agents: agents, agentSearch: ft.Agents, aerr: aerr,
		health:           health,
		selectedTaskID:   selectedTaskID,
		selectedComments: selectedComments, hasCommentsErr: hasCommentsErr,
		selectedSignals: selectedSignals, hasSignalsErr: hasSignalsErr,
		fallbacksNow: fallbacksNow,
	}
}

// applyRefreshLocked mutates the model from a refreshResult. Must be
// called inside a g.Update closure (which is the only place gocui
// allows touching view state). Held under st.mu.Lock.
//
// Detail-pane side effects:
//
//   - When the currently-selected job transitions from running to
//     terminal between snapshots, its transcript entry's loadedAt
//     is zeroed so the next selection-driven hydration refetches
//     the archive (so final-flushed events appear). The schedule
//     is queued AFTER the lock is released, via OnWorker, so we
//     don't block the apply path on the refetch.
//   - When state.jobLiveJobID no longer appears in the jobs slice
//     (filter / scope removed it), stopJobLive tears down the
//     subscription.
func (gu *Gui) applyRefreshLocked(r refreshResult) {
	var (
		refetchJobID string
		stopLive     bool
	)
	gu.st.withLock(func() {
		if r.terr == nil {
			gu.st.tasks = sortTasksByRecency(applyTaskSearch(r.tasks, r.taskSearch))
			gu.st.taskCursor = clampCursor(gu.st.taskCursor, len(gu.st.tasks))
		}
		if r.jerr == nil {
			// Detect running→terminal transition for the currently-
			// selected job BEFORE we swap the jobs slice.
			prevJob, hadPrev := gu.st.selectedJob()
			gu.st.jobs = applyJobSearch(r.jobs, r.jobsSearch)
			gu.st.jobCursor = clampCursor(gu.st.jobCursor, len(gu.st.jobs))
			gu.st.taskJobIdx = r.taskJobIdx
			if hadPrev {
				if cur, ok := gu.st.selectedJob(); ok && cur.JobID == prevJob.JobID {
					wasRunning := runstore.RunStatus(prevJob.Status) == runstore.StatusRunning
					isRunning := runstore.RunStatus(cur.Status) == runstore.StatusRunning
					if wasRunning && !isRunning {
						if te, ok := gu.st.jobTranscript[cur.JobID]; ok {
							te.loadedAt = time.Time{}
							te.runningSeen = false
						}
						refetchJobID = cur.JobID
					}
				}
			}
			// If the streamed job vanished from the slice (e.g. a
			// filter just removed it), tear down the subscription so
			// we're not holding an open SSE for a job the operator
			// can't see.
			if gu.st.jobLiveJobID != "" {
				found := false
				for i := range gu.st.jobs {
					if gu.st.jobs[i].JobID == gu.st.jobLiveJobID {
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
			gu.st.workflows = applyWfSearch(r.visibleWorkflows, r.workflowSearch)
			gu.st.workflowCursor = clampCursor(gu.st.workflowCursor, len(gu.st.workflows))
		}
		if r.aerr == nil {
			gu.st.agents = applyAgentSearch(r.agents, r.agentSearch)
			gu.st.agentCursor = clampCursor(gu.st.agentCursor, len(gu.st.agents))
		}
		gu.st.health = r.health
		gu.st.fallbacksLast = gu.st.fallbacksNow
		gu.st.fallbacksNow = r.fallbacksNow
		if r.selectedTaskID != "" && !r.hasCommentsErr && r.selectedComments != nil {
			evictCacheIfNeeded(gu.st.comments, r.selectedTaskID, commentsCacheMax)
			gu.st.comments[r.selectedTaskID] = r.selectedComments
		}
		if r.selectedTaskID != "" && !r.hasSignalsErr && r.selectedSignals != nil {
			evictCacheIfNeeded(gu.st.signals, r.selectedTaskID, commentsCacheMax)
			gu.st.signals[r.selectedTaskID] = r.selectedSignals
		}
	})
	if stopLive {
		gu.stopJobLive()
	}
	if refetchJobID != "" && gu.g != nil {
		jobID := refetchJobID
		gu.g.OnWorker(func(_ gocui.Task) error {
			gu.loadJobArchive(jobID)
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
// Supported facets:
//
//	p:<n>            priority
//	status:<status>  task status
//	wf:<name>        workflow name (resolved to wf-id via wfByName)
//	agent:<name>     matches BOTH author_id == name and
//	                 current_step.agent == name (broadest sense)
//
// Free text is matched as a substring against id + title.
func applyFacetFilter(f *datasource.TaskFilter, expr string, wfByName map[string]string) {
	tokens := strings.Fields(expr)
	var free []string
	for _, tok := range tokens {
		if !strings.Contains(tok, ":") {
			free = append(free, tok)
			continue
		}
		k, v, _ := strings.Cut(tok, ":")
		switch strings.ToLower(k) {
		case "p":
			var p int
			if _, err := fmt.Sscanf(v, "%d", &p); err == nil {
				f.Priority = &p
			}
		case "status":
			f.Statuses = []store.Status{store.Status(v)}
		case "wf":
			if id, ok := wfByName[strings.ToLower(v)]; ok {
				f.WorkflowID = id
			} else {
				// Unknown name → impossible match. Use a sentinel so the
				// caller's WHERE clause filters everything out instead of
				// silently ignoring the chip.
				f.WorkflowID = "\x00" + v
			}
		case "agent":
			f.AgentName = v
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

// filterJobsByTaskID returns the subset of jobs whose TaskID matches
// id. Used by fetchRefresh to apply scope.TaskID client-side after
// the server-side fetch dropped that filter (so the Tasks-panel
// marker index can see ALL tasks' jobs, not just the scoped one's).
// Allocates a fresh slice so the caller's input isn't aliased.
func filterJobsByTaskID(in []datasource.Job, id string) []datasource.Job {
	if id == "" {
		return in
	}
	out := make([]datasource.Job, 0, len(in))
	for i := range in {
		if in[i].TaskID == id {
			out = append(out, in[i])
		}
	}
	return out
}

func applyJobSearch(in []datasource.Job, expr string) []datasource.Job {
	needle := strings.ToLower(strings.TrimSpace(expr))
	if needle == "" {
		return in
	}
	out := make([]datasource.Job, 0, len(in))
	for _, j := range in {
		hay := strings.ToLower(j.JobID + " " + j.Status + " " + j.WorkflowName + " " + j.StepName)
		if strings.Contains(hay, needle) {
			out = append(out, j)
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
