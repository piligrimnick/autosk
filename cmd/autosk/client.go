package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"autosk/internal/daemon/rpcclient"
	"autosk/internal/projectdb"
)

// cliSource is the write-verb `source` discriminator. The daemon uses it
// to reproduce the exact dolt_commit message + behaviour of the CLI
// front end (vs. the lazy TUI, which passes "lazy").
const cliSource = "cli"

// cliClient builds an autoskd JSON-RPC client for a CLI verb: socket from
// $AUTOSK_SOCK (or the default), cwd from the process working directory,
// and the --db / $AUTOSK_DB override. autoskd is auto-spawned on first
// use by the connector.
func cliClient() (*rpcclient.Client, error) {
	return rpcclient.New(rpcclient.Options{
		DBOverride: dbOverride(),
	})
}

// ensureProject prepares the project before a verb's RPC. It preserves
// the pre-daemon openStore contract:
//
//   - A discoverable .autosk/db (override / $AUTOSK_DB / walk-up) is
//     accepted as-is; the daemon migrates it on resolve.
//   - A missing DB on a READ verb is a hard error.
//   - A missing DB on a WRITE verb runs the interactive auto-init gate
//     (AUTOSK_NO_AUTOINIT / the TTY y/n prompt / AUTOSK_AUTOINIT_ASSUME_YES),
//     then calls project.init on the daemon (which mkdirs + migrates +
//     bootstraps feature-dev-generic unless AUTOSK_AUTOINIT_SKIP_BOOTSTRAP
//     is set). On creation it prints the same `autosk: created <db>` line.
func ensureProject(ctx context.Context, cl *rpcclient.Client, writeOK bool) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	if _, rerr := projectdb.Resolve(cwd, flagDB); rerr == nil {
		return nil // discoverable; the daemon will resolve + migrate it.
	} else if !errors.Is(rerr, projectdb.ErrNotFound) {
		return rerr
	}

	if !writeOK {
		return fmt.Errorf("no .autosk/db found in this directory or any parent (run `autosk init`, or run a write command to auto-init)")
	}

	// Write verb in a fresh dir: the auto-init gate (client-side, plan §7.6).
	if os.Getenv(projectdb.EnvNoAutoInit) != "" {
		return projectdb.ErrAutoInitDisabled
	}
	if shouldPromptForAutoInit() {
		ok, perr := promptCreateDB(cwd)
		if perr != nil {
			return fmt.Errorf("read confirmation: %w", perr)
		}
		if !ok {
			return fmt.Errorf("no .autosk/db: declined to create one (run `autosk init` explicitly, or point at an existing project with --db <path> / AUTOSK_DB)")
		}
	}

	skipBootstrap := os.Getenv(EnvAutoInitSkipBootstrap) != ""
	res, err := cl.ProjectInit(ctx, skipBootstrap)
	if err != nil {
		return err
	}
	if !flagQuiet {
		fmt.Fprintf(os.Stderr, "autosk: created %s\n", res.DBPath)
	}
	// Mirror `autosk init`: report the workflow seed on the auto-init path too
	// (the bootstrap itself runs in the daemon's project.init).
	if !skipBootstrap {
		reportBootstrap(ctx, cl, res)
	}
	return nil
}

// cleanRPCError unwraps a daemon RPCError down to its bare message so the
// CLI surfaces the daemon's (Go-identical) error text without the
// `autoskd rpc error N:` transport prefix. Non-RPC errors pass through.
// Applied centrally in main() + the test runRoot so every verb that
// propagates a daemon error renders the same string the pre-daemon CLI
// did.
func cleanRPCError(err error) error {
	if err == nil {
		return nil
	}
	if apiErr, ok := rpcclient.IsAPIError(err); ok {
		return errors.New(apiErr.Message)
	}
	return err
}

// writeClient is the common preamble for a write verb: build the client
// and ensure the project exists (auto-init). Returns the ready client.
func writeClient(ctx context.Context) (*rpcclient.Client, error) {
	cl, err := cliClient()
	if err != nil {
		return nil, err
	}
	if err := ensureProject(ctx, cl, true); err != nil {
		return nil, err
	}
	return cl, nil
}

// readClient is the common preamble for a read verb: build the client and
// verify the project is discoverable (no auto-init).
func readClient(ctx context.Context) (*rpcclient.Client, error) {
	cl, err := cliClient()
	if err != nil {
		return nil, err
	}
	if err := ensureProject(ctx, cl, false); err != nil {
		return nil, err
	}
	return cl, nil
}
