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
// proto-v2 wire shapes, so the RPC datasource's wire→datasource mapping (the
// exact path `autosk lazy` renders through) is exercised in plain Go CI without
// the autoskd binary.
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
			"workflow": "feature-dev", "step": "dev",
			"blocked": false, "blocked_by": []any{},
			"blocks":        []map[string]any{{"id": "ask-000003", "status": "new"}},
			"comment_count": 2,
			"created_at":    "2023-11-14T22:15:00Z", "updated_at": "2023-11-14T22:16:40Z",
		}},
		"registry.workflow.list": []map[string]any{{
			"name": "feature-dev", "description": "",
			"first_step": "dev",
			"steps": []map[string]any{{
				"name": "dev", "status": nil,
				"targets": []map[string]any{{"step": "review"}},
			}},
		}},
		"session.list": []map[string]any{{
			"id": "session-000001", "task_id": "ask-000001",
			"workflow": "feature-dev", "step": "dev", "agent": "@a/g",
			"status": "done", "error": "",
			"started_at": "2023-11-14T22:16:11Z",
			"ended_at":   "2023-11-14T22:16:20Z",
		}},
		"meta.healthz": map[string]any{"ok": true, "workers": 0, "queued": 0, "running": 1,
			"projects": []any{}},
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
	if tk.CommentCount != 2 {
		t.Errorf("count wrong: %+v", tk)
	}
	if tk.CreatedAt.IsZero() {
		t.Errorf("created_at not parsed")
	}

	// Agents are derived from the workflows' agent steps (status == null): the
	// only agent step is `dev`.
	agents, err := ds.Agents(ctx)
	if err != nil || len(agents) != 1 || agents[0].Name != "dev" {
		t.Errorf("Agents mapping wrong: %+v err=%v", agents, err)
	}

	wfs, err := ds.Workflows(ctx)
	if err != nil || len(wfs) != 1 || wfs[0].FirstStep != "dev" || len(wfs[0].Steps) != 1 {
		t.Fatalf("Workflows mapping wrong: %+v err=%v", wfs, err)
	}
	if wfs[0].Steps[0].Targets[0] != "review" {
		t.Errorf("workflow step mapping wrong: %+v", wfs[0].Steps[0])
	}

	sessions, err := ds.Sessions(ctx, "ask-000001")
	if err != nil || len(sessions) != 1 || sessions[0].ID != "session-000001" {
		t.Fatalf("Sessions mapping wrong: %+v err=%v", sessions, err)
	}
	if sessions[0].Workflow != "feature-dev" || sessions[0].Step != "dev" {
		t.Errorf("session labels wrong: %+v", sessions[0])
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
			"id": "ask-000009", "title": "t", "description": "", "status": "new",
			"workflow": nil, "step": nil, "blocked": false,
			"blocked_by": []any{}, "blocks": []any{}, "comment_count": 0,
			"created_at": "2023-11-14T22:15:00Z", "updated_at": "2023-11-14T22:15:00Z",
		},
		"task.done": map[string]any{
			"id": "ask-1", "title": "x", "description": "", "status": "done",
			"workflow": nil, "step": nil, "blocked": false,
			"blocked_by": []any{}, "blocks": []any{}, "comment_count": 0,
			"created_at": "2023-11-14T22:15:00Z", "updated_at": "2023-11-14T22:15:00Z",
		},
		"task.comment.add": map[string]any{
			"id": "cm-1", "author": "human",
			"text": "hi", "created_at": "2023-11-14T22:15:00Z", "updated_at": "2023-11-14T22:15:00Z",
		},
	})
	ctx := context.Background()
	id, err := ds.CreateTask(ctx, "t", "")
	if err != nil || id != "ask-000009" {
		t.Errorf("CreateTask id=%q err=%v, want ask-000009/nil", id, err)
	}
	if err := ds.TaskDone(ctx, "ask-1"); err != nil {
		t.Errorf("TaskDone err = %v, want nil", err)
	}
	if err := ds.AddComment(ctx, "ask-1", "hi"); err != nil {
		t.Errorf("AddComment err = %v, want nil", err)
	}
}
