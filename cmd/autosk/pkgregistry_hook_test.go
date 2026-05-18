package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveInstallSpec_RegistryName(t *testing.T) {
	cases := []struct {
		arg, version, wantName, wantSpec string
	}{
		{"developer", "", "developer", "developer"},
		{"developer", "0.2.0", "developer", "developer@0.2.0"},
		{"@autosk/dev", "", "@autosk/dev", "@autosk/dev"},
		{"@autosk/dev", "1.0.0", "@autosk/dev", "@autosk/dev@1.0.0"},
	}
	for _, tc := range cases {
		t.Run(tc.arg+"@"+tc.version, func(t *testing.T) {
			name, spec, err := resolveInstallSpec(tc.arg, tc.version)
			if err != nil {
				t.Fatal(err)
			}
			if name != tc.wantName || spec != tc.wantSpec {
				t.Errorf("got (%q, %q), want (%q, %q)", name, spec, tc.wantName, tc.wantSpec)
			}
		})
	}
}

func TestResolveInstallSpec_LocalPath(t *testing.T) {
	dir := t.TempDir()
	pkgDir := filepath.Join(dir, "my-agent")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pj := map[string]any{"name": "fancy", "version": "0.0.1"}
	body, _ := json.Marshal(pj)
	if err := os.WriteFile(filepath.Join(pkgDir, "package.json"), body, 0o644); err != nil {
		t.Fatal(err)
	}

	// Use the absolute path (starts with /) so isLocalPath fires.
	name, spec, err := resolveInstallSpec(pkgDir, "")
	if err != nil {
		t.Fatalf("resolveInstallSpec: %v", err)
	}
	if name != "fancy" {
		t.Errorf("name = %q, want fancy", name)
	}
	if spec != pkgDir {
		t.Errorf("spec = %q, want %q", spec, pkgDir)
	}
}

func TestResolveInstallSpec_LocalPath_Relative(t *testing.T) {
	dir := t.TempDir()
	pkgDir := filepath.Join(dir, "rel-pkg")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"name": "rel-pkg-name", "version": "0.0.1"})
	if err := os.WriteFile(filepath.Join(pkgDir, "package.json"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	// Chdir so "./rel-pkg" resolves to pkgDir.
	cwd, _ := os.Getwd()
	defer os.Chdir(cwd)
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	name, spec, err := resolveInstallSpec("./rel-pkg", "")
	if err != nil {
		t.Fatalf("resolveInstallSpec: %v", err)
	}
	if name != "rel-pkg-name" {
		t.Errorf("name = %q, want rel-pkg-name", name)
	}
	if !strings.HasSuffix(spec, "rel-pkg") || !filepath.IsAbs(spec) {
		t.Errorf("spec should be absolute path ending in rel-pkg, got %q", spec)
	}
}

func TestResolveInstallSpec_LocalPathMissing(t *testing.T) {
	_, _, err := resolveInstallSpec("/nonexistent/path/to/agent", "")
	if err == nil {
		t.Fatal("expected error for missing path")
	}
}

func TestIsLocalPath(t *testing.T) {
	yes := []string{"./foo", "../foo", "/abs/path", "~/home", "~"}
	no := []string{"foo", "@scope/name", "developer", "code-reviewer", "@autosk/agent-runtime"}
	for _, s := range yes {
		if !isLocalPath(s) {
			t.Errorf("isLocalPath(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if isLocalPath(s) {
			t.Errorf("isLocalPath(%q) = true, want false", s)
		}
	}
}
