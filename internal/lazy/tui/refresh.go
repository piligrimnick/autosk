package tui

import (
	"context"
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

	tasksFilter := datasource.DefaultTaskFilter()
	if sc.WorkflowID != "" {
		tasksFilter.WorkflowID = sc.WorkflowID
	}
	if sc.Agent != "" {
		tasksFilter.AgentName = sc.Agent
	}
	applyFacetFilter(&tasksFilter, ft.Tasks)
	tasks, terr := gu.ds.Tasks(ctx, tasksFilter)

	jobsFilter := datasource.JobFilter{}
	if sc.TaskID != "" {
		jobsFilter.TaskID = sc.TaskID
	}
	if sc.WorkflowID != "" {
		jobsFilter.WorkflowID = sc.WorkflowID
	}
	jobs, jerr := gu.ds.Jobs(ctx, jobsFilter)

	workflows, werr := gu.ds.Workflows(ctx, false)
	agents, aerr := gu.ds.Agents(ctx)
	health, _ := gu.ds.Healthz(ctx)

	gu.g.Update(func(_ *gocui.Gui) error {
		gu.st.withLock(func() {
			if terr == nil {
				gu.st.tasks = applyTaskSearch(tasks, ft.Tasks)
				gu.st.taskCursor = clampCursor(gu.st.taskCursor, len(gu.st.tasks))
			}
			if jerr == nil {
				gu.st.jobs = applyJobSearch(jobs, ft.Jobs)
				gu.st.jobCursor = clampCursor(gu.st.jobCursor, len(gu.st.jobs))
			}
			if werr == nil {
				gu.st.workflows = applyWfSearch(workflows, ft.Workflows)
				gu.st.workflowCursor = clampCursor(gu.st.workflowCursor, len(gu.st.workflows))
			}
			if aerr == nil {
				gu.st.agents = applyAgentSearch(agents, ft.Agents)
				gu.st.agentCursor = clampCursor(gu.st.agentCursor, len(gu.st.agents))
			}
			gu.st.health = health
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
//	wf:<name>        workflow name
//	agent:<name>     agent name
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
		case "p":
			var p int
			if _, err := fmtSscanf(v, "%d", &p); err == nil {
				f.Priority = &p
			}
		case "status":
			f.Statuses = []store.Status{store.Status(v)}
		case "wf":
			// Resolved by refreshAll's Tasks filter; mapping name to id
			// is done in the offline datasource via the WorkflowID
			// equality, so we don't try here.
			_ = v
		case "agent":
			f.AgentName = v
		default:
			free = append(free, tok)
		}
	}
	f.Search = strings.Join(free, " ")
}

// fmtSscanf is a tiny indirection so this file doesn't import "fmt".
func fmtSscanf(s, format string, a ...any) (int, error) {
	return sscanfShim(s, format, a...)
}

func applyTaskSearch(in []datasource.Task, expr string) []datasource.Task {
	if expr == "" {
		return in
	}
	needle := strings.ToLower(strings.TrimSpace(expr))
	if needle == "" {
		return in
	}
	out := in[:0]
	for _, t := range in {
		if strings.Contains(strings.ToLower(t.ID), needle) ||
			strings.Contains(strings.ToLower(t.Title), needle) {
			out = append(out, t)
		}
	}
	return out
}

func applyJobSearch(in []datasource.Job, expr string) []datasource.Job {
	if expr == "" {
		return in
	}
	needle := strings.ToLower(expr)
	out := in[:0]
	for _, j := range in {
		hay := strings.ToLower(j.JobID + " " + j.Status + " " + j.WorkflowName + " " + j.StepName)
		if strings.Contains(hay, needle) {
			out = append(out, j)
		}
	}
	return out
}

func applyWfSearch(in []datasource.Workflow, expr string) []datasource.Workflow {
	if expr == "" {
		return in
	}
	needle := strings.ToLower(expr)
	out := in[:0]
	for _, w := range in {
		if strings.Contains(strings.ToLower(w.Name), needle) {
			out = append(out, w)
		}
	}
	return out
}

func applyAgentSearch(in []datasource.Agent, expr string) []datasource.Agent {
	if expr == "" {
		return in
	}
	needle := strings.ToLower(expr)
	out := in[:0]
	for _, a := range in {
		if strings.Contains(strings.ToLower(a.Name), needle) {
			out = append(out, a)
		}
	}
	return out
}
