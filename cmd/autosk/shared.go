package main

import (
	"os"
	"path/filepath"
)

// resolveDBOverride returns the X-Autosk-DB value (or empty) that
// every cmd/autosk client uses when talking to the daemon. Order of
// precedence:
//
//  1. The global --db flag (set via flagDB).
//  2. The AUTOSK_DB environment variable.
//
// Each candidate is absolute-path-normalised when possible so the
// daemon receives a path it can resolve regardless of the caller's
// cwd. Empty means "no override; let the daemon discover".
//
// Shared by cmd/autosk/daemon.go (daemonClient.dbOverride delegates
// to this) and cmd/autosk/attach.go (calls this directly when
// constructing client.Options) so a future change to env precedence
// or to how relative paths resolve only has to land in one place.
func resolveDBOverride() string {
	if flagDB != "" {
		if abs, err := filepath.Abs(flagDB); err == nil {
			return abs
		}
		return flagDB
	}
	if env := os.Getenv("AUTOSK_DB"); env != "" {
		if abs, err := filepath.Abs(env); err == nil {
			return abs
		}
		return env
	}
	return ""
}
