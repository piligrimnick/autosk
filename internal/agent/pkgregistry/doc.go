// Package pkgregistry manages the global npm prefix that holds
// installed autosk agent packages.
//
// The prefix lives at $AUTOSK_PACKAGES (default ~/.autosk/packages) and
// has the shape:
//
//	~/.autosk/packages/
//	  package.json              -- autosk-managed; tracks installed pkgs as deps
//	  package-lock.json         -- autosk-managed
//	  registry.json             -- autosk-managed; index of installed agent pkgs
//	  node_modules/
//	    @autosk/agent-runtime/  -- bootstrapper (auto-installed dep)
//	    @autosk/developer/      -- example standard agent
//	    @autosk/lint-fixer/     -- example custom agent (with runner)
//
// `registry.json` is the source of truth for "is this name a real
// installed agent". `package.json`/`package-lock.json` exist for
// reproducibility (`npm ci --prefix ~/.autosk/packages`).
//
// Reading agent config:
//
//	r, err := pkgregistry.Default()
//	pkg, err := r.Resolve("@autosk/developer")
//	// pkg.Runner == ""    → standard agent: spawn pi with pkg.Model/Thinking/...
//	// pkg.Runner != ""    → custom agent: bootstrap Node + tsx + runner
//
// Mutating commands (`autosk agent install/uninstall`) shell out via the
// NpmRunner interface; the default ExecNpm implementation runs `npm`
// from PATH. Tests use a fake NpmRunner that materialises the same
// on-disk shape directly.
//
// Plan: docs/plans/20260518-Agent-Packages.md.
package pkgregistry
