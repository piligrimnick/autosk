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

// pattern matches a valid id. Prefix is one-or-more lowercase letters; suffix
// is exactly 4 lowercase hex chars.
var pattern = regexp.MustCompile(`^[a-z]+-[0-9a-f]{4}$`)

// Valid reports whether s parses as a task id.
func Valid(s string) bool { return pattern.MatchString(s) }

// New returns a single fresh id with the given prefix (no uniqueness check).
func New(prefix string) (string, error) {
	if prefix == "" {
		prefix = DefaultPrefix
	}
	var buf [2]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("crypto/rand: %w", err)
	}
	return prefix + "-" + hex.EncodeToString(buf[:]), nil
}

// ExistsFunc tests whether an id already exists. Used by NewUnique.
type ExistsFunc func(id string) (bool, error)

// NewUnique returns a fresh id that is not currently in use according to
// exists. Returns ErrExhausted if no collision-free id is found within
// MaxAttempts tries.
func NewUnique(prefix string, exists ExistsFunc) (string, error) {
	if prefix == "" {
		prefix = DefaultPrefix
	}
	for i := 0; i < MaxAttempts; i++ {
		candidate, err := New(prefix)
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
