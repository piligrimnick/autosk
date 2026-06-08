// Package buildinfo holds linker-injected build metadata.
package buildinfo

// Version is the autosk version. Set via -ldflags at build time.
// Defaults to "dev" for `go run`.
var Version = "dev"

// Commit is the short git commit hash. Set via -ldflags at build time.
var Commit = "unknown"

// Backend names the storage backend the binary talks to. The Go binary
// is a pure JSON-RPC client and links no storage engine itself;
// persistence is owned by the Rust daemon (autoskd), which links doltlite.
var Backend = "autoskd"
