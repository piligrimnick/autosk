package main

import (
	"path/filepath"
	"testing"
	"time"

	"autosk/internal/agent/pkgregistry"
)

// TestBuildDaemonCore_WiresAttachHubs is a regression test for the
// pirunners-not-wired blocker: the production daemon serve command
// MUST construct *both* the registry AND the attachments counter, AND
// thread them into BOTH projectmgr.Deps AND server.Deps. Otherwise
// the lazy TUI's Live tab is permanently broken (Streaming=false,
// AttachCount=0, /input → 503, /abort → 503).
//
// We can't peek into private deps fields from outside the packages,
// so we rely on server.AttachEnabled() (added alongside this test)
// and on the returned hubs being non-nil.
func TestBuildDaemonCore_WiresAttachHubs(t *testing.T) {
	regDir := t.TempDir()
	reg, err := pkgregistry.Open(filepath.Join(regDir, "packages"), pkgregistry.WithNpm(e2eFakeNpmUDS{}))
	if err != nil {
		t.Fatalf("pkgregistry: %v", err)
	}
	if err := reg.EnsurePrefix(); err != nil {
		t.Fatalf("EnsurePrefix: %v", err)
	}

	core := buildDaemonCore(daemonCoreConfig{
		Reg:          reg,
		Workers:      1,
		PollInterval: time.Hour,
		Grace:        100 * time.Millisecond,
		IdleTimeout:  5 * time.Second,
	})

	if core.Runners == nil {
		t.Fatal("buildDaemonCore: Runners is nil; attach surface will 503")
	}
	if core.Attachments == nil {
		t.Fatal("buildDaemonCore: Attachments is nil; AttachCount never increments")
	}
	if core.Srv == nil {
		t.Fatal("buildDaemonCore: Srv is nil")
	}
	if !core.Srv.AttachEnabled() {
		t.Fatal("buildDaemonCore: server.AttachEnabled() == false; pirunners not threaded into server.Deps")
	}
}
