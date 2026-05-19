package server_test

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"autosk/internal/agent/pkgregistry"
	"autosk/internal/daemon/api"
	"autosk/internal/daemon/executor"
	"autosk/internal/daemon/projectmgr"
	"autosk/internal/daemon/scheduler"
	"autosk/internal/daemon/server"
	"autosk/internal/store/doltlite"
)

// ---- shared test plumbing -----------------------------------------------

// fakeNpm satisfies the pkgregistry npm runner interface for tests.
type fakeNpm struct{}

func (fakeNpm) Install(_ context.Context, prefix, spec string) error   { return nil }
func (fakeNpm) Uninstall(_ context.Context, prefix, name string) error { return nil }

// tempUDSDir returns a directory short enough to host a unix-domain
// socket. macOS sun_path is capped at 104 bytes.
func tempUDSDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if len(dir)+len("/daemon.sock") < 100 {
		return dir
	}
	short, err := os.MkdirTemp("/tmp", "uds-srv-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(short) })
	return short
}

// initProject creates a tmp dir containing a fresh, migrated .autosk/db.
func initProject(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".autosk"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	dbPath := filepath.Join(root, ".autosk", "db")
	ts := doltlite.New()
	if err := ts.Open(context.Background(), dbPath); err != nil {
		t.Fatalf("doltlite open: %v", err)
	}
	if err := ts.Migrate(context.Background()); err != nil {
		_ = ts.Close()
		t.Fatalf("migrate: %v", err)
	}
	if err := ts.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return root
}

// srvFixture brings up an http.Server backed by a unix listener plus
// projectmgr + scheduler. Returns a client whose Do() targets the
// socket regardless of the request URL host.
type srvFixture struct {
	sockDir  string
	sockPath string
	mgr      *projectmgr.Manager
	sched    *scheduler.Scheduler
	srv      *server.Server
	httpSrv  *http.Server
	httpCli  *http.Client
	wg       sync.WaitGroup
	closeFns []func()
}

func newSrvFixture(t *testing.T) *srvFixture {
	t.Helper()
	sockDir := tempUDSDir(t)
	sockPath := filepath.Join(sockDir, "d.sock")

	regDir := t.TempDir()
	reg, err := pkgregistry.Open(filepath.Join(regDir, "packages"), pkgregistry.WithNpm(fakeNpm{}))
	if err != nil {
		t.Fatalf("pkgregistry: %v", err)
	}
	if err := reg.EnsurePrefix(); err != nil {
		t.Fatalf("EnsurePrefix: %v", err)
	}

	sched := scheduler.New(scheduler.ExecutorFunc(func(ctx context.Context, job scheduler.Job) error {
		return nil
	}), scheduler.Config{Workers: 1})
	if err := sched.Start(context.Background()); err != nil {
		t.Fatalf("sched start: %v", err)
	}
	mgr := projectmgr.New(projectmgr.Deps{
		Sched:        sched,
		Packages:     reg,
		PollInterval: time.Hour,
		ExecCfg: executor.Config{
			Grace:       100 * time.Millisecond,
			IdleTimeout: 5 * time.Second,
		},
	})
	srv := server.New(server.Deps{Projects: mgr, Sched: sched, Workers: 2})

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	httpSrv := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 5 * time.Second}
	fx := &srvFixture{
		sockDir:  sockDir,
		sockPath: sockPath,
		mgr:      mgr,
		sched:    sched,
		srv:      srv,
		httpSrv:  httpSrv,
		httpCli: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", sockPath)
				},
			},
			Timeout: 5 * time.Second,
		},
	}
	fx.wg.Add(1)
	go func() {
		defer fx.wg.Done()
		_ = httpSrv.Serve(ln)
	}()
	fx.closeFns = append(fx.closeFns,
		func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = httpSrv.Shutdown(ctx)
		},
		func() {
			fx.wg.Wait()
		},
		func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = mgr.CloseAll(ctx)
		},
		func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = sched.Stop(ctx)
		},
		func() { _ = os.Remove(sockPath) },
	)
	t.Cleanup(fx.shutdown)
	return fx
}

func (fx *srvFixture) shutdown() {
	for _, fn := range fx.closeFns {
		fn()
	}
	fx.closeFns = nil
}

// do executes an HTTP request against the daemon socket, optionally
// setting X-Autosk-Cwd / X-Autosk-DB headers.
func (fx *srvFixture) do(t *testing.T, method, path string, cwd, db string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, "http://autosk"+path, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if cwd != "" {
		req.Header.Set("X-Autosk-Cwd", cwd)
	}
	if db != "" {
		req.Header.Set("X-Autosk-DB", db)
	}
	resp, err := fx.httpCli.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	return resp
}

// ---- tests --------------------------------------------------------------

func TestServer_VersionIsExempt(t *testing.T) {
	fx := newSrvFixture(t)
	resp := fx.do(t, http.MethodGet, "/v1/version", "", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	var v api.VersionResponse
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Version may be empty in tests (build info is set at link time);
	// the only invariant is that the route was reachable.
}

func TestServer_MissingCwdHeaderIs400(t *testing.T) {
	fx := newSrvFixture(t)
	resp := fx.do(t, http.MethodGet, "/v1/jobs", "", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 400, got %d (%s)", resp.StatusCode, body)
	}
}

func TestServer_RelativeCwdIs400(t *testing.T) {
	fx := newSrvFixture(t)
	resp := fx.do(t, http.MethodGet, "/v1/jobs", "relative/path", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 400, got %d (%s)", resp.StatusCode, body)
	}
}

// TestServer_RelativeDBHeaderIs400 covers the X-Autosk-DB sanity check
// in projectMiddleware: any non-absolute db path must be rejected
// before the manager would otherwise be asked to open it.
func TestServer_RelativeDBHeaderIs400(t *testing.T) {
	fx := newSrvFixture(t)
	root := initProject(t)
	resp := fx.do(t, http.MethodGet, "/v1/jobs", root, "relative/db")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 400, got %d (%s)", resp.StatusCode, body)
	}
}

// TestServer_DBOverrideMissingIs404 confirms that pointing X-Autosk-DB
// at a non-existent file produces a 404 instead of silently auto-
// creating a fresh database (the daemon never auto-inits).
func TestServer_DBOverrideMissingIs404(t *testing.T) {
	fx := newSrvFixture(t)
	root := initProject(t)
	missingDB := filepath.Join(t.TempDir(), ".autosk", "db") // never created.
	resp := fx.do(t, http.MethodGet, "/v1/jobs", root, missingDB)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 404, got %d (%s)", resp.StatusCode, body)
	}
	// The file must not exist as a side effect.
	if _, err := os.Stat(missingDB); !os.IsNotExist(err) {
		t.Fatalf("server created %s as a side effect of the request (auto-init leaked)", missingDB)
	}
}

func TestServer_UnknownProjectIs404(t *testing.T) {
	fx := newSrvFixture(t)
	missing := t.TempDir() // no .autosk
	resp := fx.do(t, http.MethodGet, "/v1/jobs", missing, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 404, got %d (%s)", resp.StatusCode, body)
	}
}

func TestServer_ListIsScopedToProject(t *testing.T) {
	fx := newSrvFixture(t)
	rootA := initProject(t)
	rootB := initProject(t)

	respA := fx.do(t, http.MethodGet, "/v1/jobs", rootA, "")
	defer respA.Body.Close()
	if respA.StatusCode != http.StatusOK {
		t.Fatalf("listA status=%d", respA.StatusCode)
	}
	var listA api.ListResponse
	if err := json.NewDecoder(respA.Body).Decode(&listA); err != nil {
		t.Fatalf("decode A: %v", err)
	}
	if len(listA.Jobs) != 0 {
		t.Fatalf("expected empty list for A, got %d", len(listA.Jobs))
	}

	respB := fx.do(t, http.MethodGet, "/v1/jobs", rootB, "")
	defer respB.Body.Close()
	if respB.StatusCode != http.StatusOK {
		t.Fatalf("listB status=%d", respB.StatusCode)
	}
}

func TestServer_SubmitRouteIsRemoved(t *testing.T) {
	fx := newSrvFixture(t)
	root := initProject(t)
	req, _ := http.NewRequest(http.MethodPost, "http://autosk/v1/jobs", strings.NewReader(`{}`))
	req.Header.Set("X-Autosk-Cwd", root)
	resp, err := fx.httpCli.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	// ServeMux returns 405 (method not allowed) when the path exists
	// for other verbs; 404 if not registered. Either proves the route
	// is gone — what we must not see is a 2xx or 501.
	if resp.StatusCode != http.StatusMethodNotAllowed && resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 404/405 for POST /v1/jobs, got %d (%s)", resp.StatusCode, body)
	}
}

func TestServer_HealthScopedByDefault(t *testing.T) {
	fx := newSrvFixture(t)
	root := initProject(t)
	resp := fx.do(t, http.MethodGet, "/v1/healthz", root, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	var h api.HealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if h.ProjectRoot == "" {
		t.Errorf("expected project_root populated for scoped health, got empty")
	}
	if len(h.Projects) != 0 {
		t.Errorf("expected no Projects array on scoped health, got %d", len(h.Projects))
	}
}

func TestServer_HealthAggregateExempt(t *testing.T) {
	fx := newSrvFixture(t)
	rootA := initProject(t)
	rootB := initProject(t)
	// Touch both so they're loaded.
	for _, r := range []string{rootA, rootB} {
		resp := fx.do(t, http.MethodGet, "/v1/healthz", r, "")
		resp.Body.Close()
	}

	resp := fx.do(t, http.MethodGet, "/v1/healthz?all=true", "", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	var h api.HealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(h.Projects) < 2 {
		t.Fatalf("expected ≥2 projects in aggregate, got %d", len(h.Projects))
	}
}

func TestServer_HealthScopedRequiresCwd(t *testing.T) {
	fx := newSrvFixture(t)
	resp := fx.do(t, http.MethodGet, "/v1/healthz", "", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}
