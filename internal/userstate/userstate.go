// Package userstate manages the per-user `~/.autosk/state.json`
// file that lazy uses to remember operator-scoped UX state between
// sessions.
//
// Today the only field is `last_seen_changelog` (consumed by the
// changelog-modal-on-first-run feature) but the struct is left open
// so future per-user preferences (theme override, dismissed
// tooltips, etc.) can land without touching call sites.
//
// File layout — strict invariants:
//
//   - Path: $HOME/.autosk/state.json (co-located with daemon.sock
//     so a single `chmod 0700` on ~/.autosk covers both files).
//   - Permissions: 0600 on the file, 0700 on the parent directory.
//     The file holds nothing sensitive today, but the per-user
//     scope means tighter perms keep weird multi-user setups
//     predictable.
//   - Atomic write: tempfile in the same directory + os.Rename, so
//     a crash mid-write can never leave the consumer reading half
//     of one struct and half of another.
//
// Missing-file semantics: Load on a non-existent state.json (or a
// non-existent parent directory) returns a zero State{} and a nil
// error. Callers treat a zero value as "brand-new operator". This
// is the contract the changelog-on-first-run flow depends on.
package userstate

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// State is the persisted blob. Add new fields with `omitempty` so
// the on-disk JSON stays small and so an operator who reads it can
// see at a glance which fields they've actually exercised.
type State struct {
	// LastSeenChangelog is the bare semver (no leading "v") of the
	// newest release whose changelog popup has been dismissed by
	// the operator on this machine. The lazy startup hook compares
	// it against `changelog.Latest(changelog.Embedded())` and
	// fires the popup when the embedded set has anything strictly
	// newer.
	LastSeenChangelog string `json:"last_seen_changelog,omitempty"`
}

// Path returns the canonical state.json location for the current
// user. Honors $AUTOSK_STATE_FILE for tests + golden runs that
// need to redirect the file without touching $HOME.
//
// Falls back to ./.autosk/state.json (relative to the CWD) when
// $HOME is unset — the same fallback shape `defaultSockPath` in
// cmd/autosk/daemon.go uses for the socket, so a HOME-less
// environment writes both into the same directory.
func Path() string {
	if env := os.Getenv("AUTOSK_STATE_FILE"); env != "" {
		return env
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(".autosk", "state.json")
	}
	return filepath.Join(home, ".autosk", "state.json")
}

// Load reads state.json from Path() and returns the parsed State.
// A missing file (ENOENT on either the file or its parent dir)
// returns a zero State{} with nil error — the first-run path.
//
// A malformed JSON payload is surfaced as an error: it indicates
// either a hand-edit gone wrong or a partial-write window that
// pre-dated the atomic-rename guarantee, and the caller (the lazy
// startup hook) should treat that as "leave the file alone, skip
// the popup" rather than silently overwriting the operator's
// state.
func Load() (State, error) {
	return loadFrom(Path())
}

func loadFrom(path string) (State, error) {
	buf, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return State{}, nil
		}
		return State{}, fmt.Errorf("userstate: read %s: %w", path, err)
	}
	var s State
	if err := json.Unmarshal(buf, &s); err != nil {
		return State{}, fmt.Errorf("userstate: parse %s: %w", path, err)
	}
	return s, nil
}

// Save writes s to Path() via tempfile + os.Rename.
//
// Permission rules:
//   - Parent directory: best-effort MkdirAll with 0700 (no error if
//     the directory already exists with different perms — we don't
//     own ~/.autosk's mode, only the file's).
//   - Tempfile: created with 0600 via os.OpenFile so a panic
//     between create and rename never leaves a world-readable
//     leftover.
//   - Final file: 0600 enforced post-rename (Linux preserves the
//     source perms across rename; macOS does the same in practice,
//     but the explicit chmod is cheap insurance).
//
// On rename failure the tempfile is removed so the directory
// doesn't accumulate `state.json.tmp*` leftovers across crashes.
func Save(s State) error {
	return saveTo(Path(), s)
}

func saveTo(path string, s State) error {
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("userstate: mkdir %s: %w", dir, err)
		}
	}
	buf, err := json.MarshalIndent(&s, "", "  ")
	if err != nil {
		return fmt.Errorf("userstate: marshal: %w", err)
	}
	// Trailing newline — pretty for `cat ~/.autosk/state.json`.
	buf = append(buf, '\n')
	tmp, err := os.CreateTemp(dir, ".state.json.tmp-*")
	if err != nil {
		return fmt.Errorf("userstate: create tempfile in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}
	if _, err := tmp.Write(buf); err != nil {
		cleanup()
		return fmt.Errorf("userstate: write %s: %w", tmpPath, err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return fmt.Errorf("userstate: chmod %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("userstate: close %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("userstate: rename %s -> %s: %w", tmpPath, path, err)
	}
	// Final chmod is a cosmetic belt-and-braces; the tempfile was
	// already 0600, and rename preserves perms on POSIX. We still
	// do it so a future filesystem with unusual rename semantics
	// (or a test that touches the file mid-write) lands on the
	// documented mode.
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("userstate: chmod %s: %w", path, err)
	}
	return nil
}
