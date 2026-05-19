package uds_test

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"autosk/internal/daemon/uds"
)

// tempUDSDir returns a directory short enough to host a unix-domain
// socket. On macOS sun_path is capped at 104 bytes; t.TempDir() under
// /var/folders/... already eats 90+ characters before we add the
// socket name. Fall back to /tmp when the standard TempDir is too long.
func tempUDSDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if len(dir)+len("/daemon.sock") < 100 {
		return dir
	}
	short, err := os.MkdirTemp("/tmp", "uds-")
	if err != nil {
		t.Fatalf("MkdirTemp /tmp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(short) })
	return short
}

func TestListen_BindsAndChmods(t *testing.T) {
	dir := tempUDSDir(t)
	path := filepath.Join(dir, "sub", "daemon.sock")
	ln, err := uds.Listen(path)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()
	defer uds.Cleanup(path)

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("socket perm = %o, want 0600", got)
	}
	// Parent dir should be 0700 (we created it).
	pdir := filepath.Dir(path)
	pfi, err := os.Stat(pdir)
	if err != nil {
		t.Fatalf("stat parent: %v", err)
	}
	if got := pfi.Mode().Perm(); got != 0o700 {
		t.Errorf("parent dir perm = %o, want 0700", got)
	}
}

func TestListen_LivePeerErrors(t *testing.T) {
	dir := tempUDSDir(t)
	path := filepath.Join(dir, "daemon.sock")
	ln1, err := uds.Listen(path)
	if err != nil {
		t.Fatalf("Listen #1: %v", err)
	}
	defer ln1.Close()
	defer uds.Cleanup(path)

	// Spin a tiny accept loop so Dial succeeds during the probe.
	go func() {
		for {
			c, err := ln1.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	_, err = uds.Listen(path)
	if err == nil {
		t.Fatal("expected second Listen to fail with ErrAlreadyRunning")
	}
	if !errors.Is(err, uds.ErrAlreadyRunning) {
		t.Fatalf("expected ErrAlreadyRunning, got %v", err)
	}
}

func TestListen_StaleSocketReused(t *testing.T) {
	dir := tempUDSDir(t)
	path := filepath.Join(dir, "daemon.sock")

	// Bind a real listener, ask Go not to unlink on close, then close.
	// The socket file remains but no peer accepts → must look stale.
	ln1, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	if ul, ok := ln1.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}
	if err := ln1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected stale socket to remain on disk, got %v", err)
	}

	ln2, err := uds.Listen(path)
	if err != nil {
		t.Fatalf("Listen #2 (reuse stale): %v", err)
	}
	defer ln2.Close()
	defer uds.Cleanup(path)
}

func TestCleanup_NoEntIsNoOp(t *testing.T) {
	dir := tempUDSDir(t)
	path := filepath.Join(dir, "absent.sock")
	if err := uds.Cleanup(path); err != nil {
		t.Fatalf("Cleanup on missing path: %v", err)
	}
}

func TestListen_AcceptsRealConnections(t *testing.T) {
	dir := tempUDSDir(t)
	path := filepath.Join(dir, "daemon.sock")
	ln, err := uds.Listen(path)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()
	defer uds.Cleanup(path)

	done := make(chan struct{})
	go func() {
		defer close(done)
		c, err := ln.Accept()
		if err != nil {
			return
		}
		_ = c.Close()
	}()
	c, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	_ = c.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Accept never returned")
	}
}
