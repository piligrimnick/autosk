// Package buildinfo holds linker-injected build metadata.
package buildinfo

// Version is the autosk version. Set via -ldflags at build time.
// Defaults to "dev" for `go run`.
var Version = "dev"

// Commit is the short git commit hash. Set via -ldflags at build time.
var Commit = "unknown"

// Backend names the storage backend the binary was compiled against.
// "doltlite" for the default build; "doltserver" when built with -tags doltserver.
var Backend = "doltlite"
