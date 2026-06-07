package datasource

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
	"time"

	"autosk/internal/daemon/rpcclient"
	"autosk/internal/store"
)

// fakeDaemon is a minimal line-delimited JSON-RPC server returning canned
// autosk-proto wire shapes, so the RPC datasource's wire→datasource mapping
// (the exact path `autosk lazy --rpc` renders through) is exercised in plain Go
// CI without the autoskd binary.
func fakeDaemon(t *testing.T, results map[string]any) *RPC {
	t.Helper()
	dir := t.TempDir()
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				line, err := bufio.NewReader(c).ReadBytes('\n')
				if err != nil {
					return
				}
				var req struct {
					ID     uint64 `json:"id"`
					Method string `json:"method"`
				}
				_ = json.Unmarshal(line, &req)
				resp := map[string]any{"id": req.ID}
				if r, ok := results[req.Method]; ok {
					resp["result"] = r
				} else {
					resp["result"] = []any{}
				}
				b, _ := json.Marshal(resp)
				_, _ = c.Write(append(b, '\n'))
			}(conn)
		}
	}()
	cli, err := rpcclient.New(rpcclient.Options{Sock: sock, Cwd: "/repo", NoAutoSpawn: true})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return NewRPC(cli)
}

func TestRPC_MapsReadSurface(t *testing.T) {
	ds := fakeDaemon(t, map[string]any{
		"task.list": []map[string]any{{
			"id": "ask-000001", "title": "Build", "description": "d", "status": "work",
			"priority": 1, "author_id": "ag-0001", "author_name": "human",
			"workflow_id": "wf-0001", "workflow_name": "feature-dev",
			"current_step_id": "st-0001", "step_name": "dev", "agent_name": "@a/g",
			"blocked": false, "blocked_by": []any{},
			"blocks":        []map[string]any{{"id": "ask-000003", "status": "new"}},
			"comment_count": 2, "metadata": map[string]any{"k": "v"},
			"created_at": "2023-11-14T22:15:00Z", "updated_at": "2023-11-14T22:16:40Z",
		}},
		"agent.list": []map[string]any{
			{"id": "ag-0001", "name": "human", "is_human": true, "source": "builtin",
				"version": "", "model": "", "thinking": "", "extra_args": []any{},
				"pi_skills": []any{}, "pi_ext": []any{}, "tasks_owned": 2},
		},
		"workflow.list": []map[string]any{{
			"id": "wf-0001", "name": "feature-dev", "description": "",
			"is_synthetic": false, "first_step": "dev",
			"steps": []map[string]any{{
				"id": "st-0001", "name": "dev", "agent_name": "@a/g",
				"next_steps": []any{"review"}, "next_status": []any{}, "task_count": 1,
			}},
			"task_count": 1, "isolation": "worktree",
			"non_terminal_task_count": 1,
			"non_terminal_tasks":      []map[string]any{{"id": "ask-000001", "status": "work", "step_name": "dev"}},
		}},
		"job.list": []map[string]any{{
			"job_id": "job-000001", "task_id": "ask-000001", "step_id": "st-0001",
			"status": "done", "transition_id": 1, "corrections_used": 0, "max_corrections": 3,
			"created_at": "2023-11-14T22:16:10Z", "started_at": "2023-11-14T22:16:11Z",
			"finished_at": "2023-11-14T22:16:20Z", "duration_ms": 9000,
			"attach_count": 0, "streaming": false,
			"workflow_name": "feature-dev", "step_name": "dev", "agent_name": "@a/g",
		}},
		"healthz": map[string]any{"ok": true, "workers": 0, "queued": 0, "running": 1,
			"db_path": "/repo/.autosk/db", "project_root": "/repo", "projects": []any{}},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	tasks, err := ds.Tasks(ctx, DefaultTaskFilter())
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("want 1 task, got %d", len(tasks))
	}
	tk := tasks[0]
	if tk.Status != store.StatusWork || tk.WorkflowName != "feature-dev" || tk.StepName != "dev" {
		t.Errorf("task derived fields wrong: %+v", tk)
	}
	if len(tk.Blocks) != 1 || tk.Blocks[0].ID != "ask-000003" || tk.Blocks[0].Status != store.StatusNew {
		t.Errorf("blocks mapping wrong: %+v", tk.Blocks)
	}
	if tk.CommentCount != 2 || tk.Metadata["k"] != "v" {
		t.Errorf("count/metadata wrong: %+v", tk)
	}
	if tk.CreatedAt.IsZero() {
		t.Errorf("created_at not parsed")
	}

	agents, err := ds.Agents(ctx)
	if err != nil || len(agents) != 1 || !agents[0].IsHuman || agents[0].Source != "builtin" {
		t.Errorf("Agents mapping wrong: %+v err=%v", agents, err)
	}

	wfs, err := ds.Workflows(ctx, true)
	if err != nil || len(wfs) != 1 || wfs[0].FirstStep != "dev" || len(wfs[0].Steps) != 1 {
		t.Fatalf("Workflows mapping wrong: %+v err=%v", wfs, err)
	}
	if wfs[0].Steps[0].NextSteps[0] != "review" || wfs[0].Isolation != "worktree" {
		t.Errorf("workflow step mapping wrong: %+v", wfs[0].Steps[0])
	}

	jobs, err := ds.Jobs(ctx, JobFilter{})
	if err != nil || len(jobs) != 1 || jobs[0].JobID != "job-000001" || jobs[0].DurationMS != 9000 {
		t.Fatalf("Jobs mapping wrong: %+v err=%v", jobs, err)
	}
	if jobs[0].WorkflowName != "feature-dev" || jobs[0].StepName != "dev" {
		t.Errorf("job labels wrong: %+v", jobs[0])
	}

	h, err := ds.Healthz(ctx)
	if err != nil || !h.IsOK() || h.Running != 1 {
		t.Errorf("Healthz mapping wrong: %+v err=%v", h, err)
	}
}

// TestRPC_WritesDispatch confirms the RPC datasource now ISSUES write RPCs
// (Phase 3a) rather than rejecting them — the inverse of the old Phase-1
// read-only assertion. The fake daemon returns canned wire shapes for the write
// methods; the datasource must dispatch and map them without error.
func TestRPC_WritesDispatch(t *testing.T) {
	ds := fakeDaemon(t, map[string]any{
		"task.create": map[string]any{
			"id": "ask-000009", "title": "t", "status": "new", "priority": 2,
			"blocked_by": []any{}, "blocks": []any{},
			"created_at": "2023-11-14T22:15:00Z", "updated_at": "2023-11-14T22:15:00Z",
		},
		"task.setStatus": map[string]any{
			"id": "ask-1", "title": "x", "status": "done",
			"blocked_by": []any{}, "blocks": []any{},
			"created_at": "2023-11-14T22:15:00Z", "updated_at": "2023-11-14T22:15:00Z",
		},
		"comment.add": map[string]any{
			"id": 1, "task_id": "ask-1", "author_id": "ag-0001", "author_name": "human",
			"text": "hi", "created_at": "2023-11-14T22:15:00Z",
		},
	})
	ctx := context.Background()
	id, err := ds.CreateTask(ctx, "t", "", 2)
	if err != nil || id != "ask-000009" {
		t.Errorf("CreateTask id=%q err=%v, want ask-000009/nil", id, err)
	}
	if err := ds.UpdateStatus(ctx, "ask-1", store.StatusDone); err != nil {
		t.Errorf("UpdateStatus err = %v, want nil", err)
	}
	if err := ds.AddComment(ctx, "ask-1", "hi"); err != nil {
		t.Errorf("AddComment err = %v, want nil", err)
	}
}
