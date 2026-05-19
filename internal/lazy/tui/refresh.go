package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jesseduffield/gocui"

	"autosk/internal/lazy/datasource"
	"autosk/internal/store"
)

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

	tasksFilter := datasource.DefaultTaskFilter()
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

	jobsFilter := datasource.JobFilter{}
	if sc.TaskID != "" {
		jobsFilter.TaskID = sc.TaskID
	}
	if sc.WorkflowID != "" {
		jobsFilter.WorkflowID = sc.WorkflowID
	}
	jobs, jerr := gu.ds.Jobs(ctx, jobsFilter)
	if jerr != nil {
		dlog("refresh: ds.Jobs: %v", jerr)
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

	// Hydrate the comments cache for the currently-selected task so
	// the Tasks-detail pane in the dashboard can show the last ≤5
	// comments per design plan §4. Cheap: one DB round-trip per
	// cursor move + per refresh tick.
	var selectedTaskID string
	gu.st.withRLock(func() {
		if t, ok := gu.st.selectedTask(); ok {
			selectedTaskID = t.ID
		}
	})
	var selectedComments []datasource.Comment
	if selectedTaskID != "" {
		selectedComments, _ = gu.ds.Comments(ctx, selectedTaskID)
	}

	gu.g.Update(func(_ *gocui.Gui) error {
		gu.st.withLock(func() {
			if terr == nil {
				// The datasource already applied tasksFilter.Search
				// (post-facet free-text). Do NOT re-filter with the raw
				// expression here: that would re-apply 'p:1' or 'wf:foo'
				// as substring matches against id+title and empty the
				// panel. Use the parsed Search remainder only.
				gu.st.tasks = applyTaskSearch(tasks, tasksFilter.Search)
				gu.st.taskCursor = clampCursor(gu.st.taskCursor, len(gu.st.tasks))
			}
			if jerr == nil {
				gu.st.jobs = applyJobSearch(jobs, ft.Jobs)
				gu.st.jobCursor = clampCursor(gu.st.jobCursor, len(gu.st.jobs))
			}
			if werr == nil {
				gu.st.workflows = applyWfSearch(visibleWorkflows, ft.Workflows)
				gu.st.workflowCursor = clampCursor(gu.st.workflowCursor, len(gu.st.workflows))
			}
			if aerr == nil {
				gu.st.agents = applyAgentSearch(agents, ft.Agents)
				gu.st.agentCursor = clampCursor(gu.st.agentCursor, len(gu.st.agents))
			}
			gu.st.health = health
			if selectedTaskID != "" && selectedComments != nil {
				gu.st.comments[selectedTaskID] = selectedComments
			}
		})
		return nil
	})
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
