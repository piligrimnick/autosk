package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"autosk/internal/daemon/rpcclient"
	"autosk/internal/projectdb"
)

// envAgentName names the agent invoking the CLI (default "human"). Used as the
// default comment author.
const envAgentName = "AUTOSK_AGENT"

// callerAgentName returns the name of the agent the CLI is running as.
func callerAgentName() string {
	name := strings.TrimSpace(os.Getenv(envAgentName))
	if name == "" {
		return "human"
	}
	return name
}

// cliClient builds an autoskd JSON-RPC client for a CLI verb: socket from
// $AUTOSK_SOCK (or the default), cwd from the process working directory.
// autoskd is auto-spawned on first use by the connector.
func cliClient() (*rpcclient.Client, error) {
	return rpcclient.New(rpcclient.Options{})
}

// ensureProject prepares the project before a verb's RPC. A discoverable
// .autosk/ project (walk-up from cwd) is accepted as-is. A missing project on a
// READ verb is a hard error; on a WRITE verb it runs the interactive auto-init
// gate (AUTOSK_NO_AUTOINIT / TTY y/n / AUTOSK_AUTOINIT_ASSUME_YES) then calls
// project.init on the daemon (which lays down the .autosk/ skeleton and
// registers the project — no DB, no seeding; feature-dev ships bundled).
func ensureProject(ctx context.Context, cl *rpcclient.Client, writeOK bool) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	if _, rerr := projectdb.ResolveRoot(cwd); rerr == nil {
		return nil // discoverable; the daemon resolves it by walk-up.
	} else if !errors.Is(rerr, projectdb.ErrNotFound) {
		return rerr
	}

	if !writeOK {
		return fmt.Errorf("no .autosk/ project found in this directory or any parent (run `autosk init`, or run a write command to auto-init)")
	}

	// Write verb in a fresh dir: the client-side auto-init gate.
	if os.Getenv(projectdb.EnvNoAutoInit) != "" {
		return projectdb.ErrAutoInitDisabled
	}
	if shouldPromptForAutoInit() {
		ok, perr := promptCreateProject(cwd)
		if perr != nil {
			return fmt.Errorf("read confirmation: %w", perr)
		}
		if !ok {
			return fmt.Errorf("no .autosk/ project: declined to create one (run `autosk init` explicitly)")
		}
	}

	info, err := cl.ProjectInit(ctx)
	if err != nil {
		return err
	}
	if !flagQuiet {
		fmt.Fprintf(os.Stderr, "autosk: created %s/.autosk\n", info.Root)
	}
	return nil
}

// cleanRPCError unwraps a daemon RPCError down to its bare message so the CLI
// surfaces the daemon's error text without the transport prefix.
func cleanRPCError(err error) error {
	if err == nil {
		return nil
	}
	if apiErr, ok := rpcclient.IsAPIError(err); ok {
		return errors.New(apiErr.Message)
	}
	return err
}

// writeClient builds the client and ensures the project exists (auto-init).
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

// readClient builds the client and verifies the project is discoverable.
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
