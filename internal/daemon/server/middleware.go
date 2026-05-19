package server

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"

	"autosk/internal/daemon/projectmgr"
)

// projectKeyT is the unexported context key type for the resolved
// *projectmgr.Project.
type projectKeyT struct{}

// projectFromCtx returns the *projectmgr.Project the projectMiddleware
// attached, or nil if the handler is exempt and ran without resolving.
func projectFromCtx(ctx context.Context) *projectmgr.Project {
	v, _ := ctx.Value(projectKeyT{}).(*projectmgr.Project)
	return v
}

// isExempt reports whether the request bypasses project resolution.
//
//   - GET /v1/version          — daemon-level metadata.
//   - GET /v1/healthz?all=true — aggregated health across projects.
//
// /v1/healthz without ?all=true still requires the project header
// (it scopes Queued/Running to that project).
func isExempt(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	switch r.URL.Path {
	case "/v1/version":
		return true
	case "/v1/healthz":
		return strings.EqualFold(r.URL.Query().Get("all"), "true")
	}
	return false
}

// projectMiddleware parses the X-Autosk-Cwd / X-Autosk-DB headers,
// resolves the project through the manager, and attaches the resulting
// *Project to the request context.
//
// Errors map as follows:
//
//   - missing/non-absolute X-Autosk-Cwd → 400.
//   - ErrInvalidCwd                     → 400.
//   - ErrProjectNotFound                → 404.
//   - any other open error              → 500.
func (s *Server) projectMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isExempt(r) {
			next.ServeHTTP(w, r)
			return
		}
		cwd := r.Header.Get("X-Autosk-Cwd")
		if cwd == "" {
			writeError(w, http.StatusBadRequest,
				"X-Autosk-Cwd header required (absolute path to a project root)", nil)
			return
		}
		if !filepath.IsAbs(cwd) {
			writeError(w, http.StatusBadRequest,
				"X-Autosk-Cwd must be an absolute path",
				map[string]any{"cwd": cwd})
			return
		}
		db := r.Header.Get("X-Autosk-DB")
		if db != "" && !filepath.IsAbs(db) {
			writeError(w, http.StatusBadRequest,
				"X-Autosk-DB must be an absolute path",
				map[string]any{"db": db})
			return
		}
		proj, err := s.deps.Projects.Resolve(r.Context(), cwd, db)
		if err != nil {
			switch {
			case errors.Is(err, projectmgr.ErrInvalidCwd):
				writeError(w, http.StatusBadRequest, err.Error(), map[string]any{"cwd": cwd})
			case errors.Is(err, projectmgr.ErrProjectNotFound):
				writeError(w, http.StatusNotFound, "no .autosk/db found from "+cwd,
					map[string]any{"cwd": cwd})
			default:
				writeError(w, http.StatusInternalServerError, "open_project: "+err.Error(),
					map[string]any{"cwd": cwd})
			}
			return
		}
		ctx := context.WithValue(r.Context(), projectKeyT{}, proj)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
