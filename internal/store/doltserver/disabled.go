//go:build !doltserver

// Package doltserver — disabled marker. Build with `-tags doltserver` to
// activate the (currently stub) implementation.
package doltserver

// Available reports whether this build supports the dolt-server backend.
const Available = false
