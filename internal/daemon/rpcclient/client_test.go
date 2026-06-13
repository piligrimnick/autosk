package rpcclient

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
	"time"
)

// fakeServer is a minimal line-delimited JSON-RPC server used to test the Go
// client transport (serialization, selector injection, error decoding) without
// the real autoskd binary, so this runs in plain Go CI.
func fakeServer(t *testing.T, handle func(method string, params map[string]any) (any, *RPCError)) string {
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
				r := bufio.NewReader(c)
				line, err := r.ReadBytes('\n')
				if err != nil {
					return
				}
				var req struct {
					ID     uint64         `json:"id"`
					Method string         `json:"method"`
					Params map[string]any `json:"params"`
				}
				_ = json.Unmarshal(line, &req)
				result, rerr := handle(req.Method, req.Params)
				resp := map[string]any{"id": req.ID}
				if rerr != nil {
					resp["error"] = rerr
				} else {
					resp["result"] = result
				}
				b, _ := json.Marshal(resp)
				_, _ = c.Write(append(b, '\n'))
			}(conn)
		}
	}()
	return sock
}

func TestClient_VersionAndSelector(t *testing.T) {
	var gotCwd string
	sock := fakeServer(t, func(method string, params map[string]any) (any, *RPCError) {
		switch method {
		case "meta.version":
			return map[string]any{"version": "0.1.0-phase1", "commit": "abc"}, nil
		case "task.list":
			gotCwd, _ = params["cwd"].(string)
			return []map[string]any{{
				"id": "ask-000001", "title": "t", "description": "", "status": "new",
				"workflow": nil, "step": nil,
				"blocked": false, "blocked_by": []any{}, "blocks": []any{}, "comment_count": 0,
				"created_at": "2023-11-14T22:15:00Z", "updated_at": "2023-11-14T22:15:00Z",
			}}, nil
		default:
			return nil, &RPCError{Code: CodeMethodNotFound, Message: "unknown"}
		}
	})

	cli, err := New(Options{Sock: sock, Cwd: "/repo", NoAutoSpawn: true})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	v, err := cli.Version(ctx)
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if v.Version != "0.1.0-phase1" {
		t.Errorf("version = %q", v.Version)
	}

	tasks, err := cli.Tasks(ctx, TaskListFilter{})
	if err != nil {
		t.Fatalf("tasks: %v", err)
	}
	if len(tasks) != 1 || tasks[0].ID != "ask-000001" {
		t.Errorf("tasks = %+v", tasks)
	}
	if tasks[0].CreatedAt.IsZero() {
		t.Errorf("created_at did not decode from RFC3339")
	}
	if gotCwd != "/repo" {
		t.Errorf("selector cwd not injected: %q", gotCwd)
	}
}

func TestClient_ErrorDecoding(t *testing.T) {
	sock := fakeServer(t, func(method string, params map[string]any) (any, *RPCError) {
		return nil, &RPCError{Code: CodeProjectNotFound, Message: "no .autosk/ project"}
	})
	cli, err := New(Options{Sock: sock, Cwd: "/repo", NoAutoSpawn: true})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = cli.Agents(ctx)
	if err == nil {
		t.Fatal("expected error")
	}
	ae, ok := IsAPIError(err)
	if !ok {
		t.Fatalf("expected RPCError, got %T: %v", err, err)
	}
	if ae.Code != CodeProjectNotFound {
		t.Errorf("code = %d", ae.Code)
	}
}

func TestClient_NoAutoSpawnFailsFast(t *testing.T) {
	dir := t.TempDir()
	cli, err := New(Options{Sock: filepath.Join(dir, "absent.sock"), Cwd: "/repo", NoAutoSpawn: true})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := cli.Version(ctx); err == nil {
		t.Fatal("expected dial failure when daemon absent and auto-spawn disabled")
	}
}
