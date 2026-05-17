// Package id generates short, hash-based task ids like "as-a1b2".
//
// Format: <prefix>-<4 lowercase hex chars>. The hex is 16 bits of entropy
// from crypto/rand. Collisions are detected and retried by the caller via
// NewUnique. For autosk's expected scale (hundreds-to-low-thousands of tasks),
// the birthday-paradox space (65536) is ample.
package id

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// DefaultPrefix is used when none is configured.
const DefaultPrefix = "as"

// MaxAttempts caps the collision retry loop in NewUnique.
const MaxAttempts = 16

// DefaultBytes is the random byte count used by New / NewUnique (2 bytes
// = 4 hex chars, matching the canonical "as-XXXX" format).
const DefaultBytes = 2

// pattern matches a valid id. Prefix is one-or-more lowercase letters;
// suffix is an even number (≥4) of lowercase hex chars: 4 for tasks,
// 6 for jobs, etc.
var pattern = regexp.MustCompile(`^[a-z]+-[0-9a-f]{4}([0-9a-f]{2})*$`)

// Valid reports whether s parses as a task id.
func Valid(s string) bool { return pattern.MatchString(s) }

// New returns a single fresh id with the given prefix and DefaultBytes of
// entropy (4 hex chars). Use NewN for a wider suffix.
func New(prefix string) (string, error) {
	return NewN(prefix, DefaultBytes)
}

// NewN returns a single fresh id with `bytes` bytes of crypto/rand entropy
// (2*bytes hex chars). No uniqueness check.
func NewN(prefix string, bytes int) (string, error) {
	if prefix == "" {
		prefix = DefaultPrefix
	}
	if bytes <= 0 {
		bytes = DefaultBytes
	}
	buf := make([]byte, bytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("crypto/rand: %w", err)
	}
	return prefix + "-" + hex.EncodeToString(buf), nil
}

// ExistsFunc tests whether an id already exists. Used by NewUnique.
type ExistsFunc func(id string) (bool, error)

// NewUnique returns a fresh id (DefaultBytes wide) that is not currently in
// use according to exists. Returns ErrExhausted if no collision-free id is
// found within MaxAttempts tries.
func NewUnique(prefix string, exists ExistsFunc) (string, error) {
	return NewUniqueN(prefix, DefaultBytes, exists)
}

// NewUniqueN is NewUnique with a configurable suffix width.
func NewUniqueN(prefix string, bytes int, exists ExistsFunc) (string, error) {
	if prefix == "" {
		prefix = DefaultPrefix
	}
	if bytes <= 0 {
		bytes = DefaultBytes
	}
	for i := 0; i < MaxAttempts; i++ {
		candidate, err := NewN(prefix, bytes)
		if err != nil {
			return "", err
		}
		taken, err := exists(candidate)
		if err != nil {
			return "", err
		}
		if !taken {
			return candidate, nil
		}
	}
	return "", ErrExhausted
}

// ErrExhausted is returned when NewUnique can't find a free id in MaxAttempts.
// Indicates the id space is saturated for the configured prefix.
var ErrExhausted = errors.New("id space exhausted (consider widening the prefix or suffix length)")

// Prefix extracts the prefix portion of an id, or "" if invalid.
func Prefix(id string) string {
	idx := strings.IndexByte(id, '-')
	if idx <= 0 {
		return ""
	}
	return id[:idx]
}
