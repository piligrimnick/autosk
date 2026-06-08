package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// EnvAutoInitSkipBootstrap, when set to a non-empty value, suppresses
// the workflow bootstrap step that openStore would otherwise run after
// auto-creating .autosk/db.
//
// Explicit `autosk init` is NOT affected by this env — it has its own
// --skip-bootstrap flag. The env exists so test helpers and scripted
// pipelines that rely on the silent non-TTY auto-init path can opt
// out of the npm-touching bootstrap step without authoring a workflow
// file.
const EnvAutoInitSkipBootstrap = "AUTOSK_AUTOINIT_SKIP_BOOTSTRAP"

// EnvAutoInitAssumeYes, when set to a non-empty value, suppresses the
// interactive y/n prompt and proceeds as if the user answered "y".
// Intended for automation that happens to run with a TTY attached
// (e.g. tmux, IDE terminals) but does not want the prompt.
const EnvAutoInitAssumeYes = "AUTOSK_AUTOINIT_ASSUME_YES"

// isInteractiveFn reports whether the current invocation has a real
// terminal attached on both stdin (so we can read the answer) and
// stderr (so the user can see the prompt). It is a package-level var
// so tests can stub it.
var isInteractiveFn = func() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stderr.Fd()))
}

// confirmReader is the source of the y/n reply. Defaults to os.Stdin;
// tests swap it for an in-memory pipe.
var confirmReader io.Reader = os.Stdin

// shouldPromptForAutoInit decides whether to surface the interactive
// y/n prompt. Returns false when the output mode is machine-oriented
// (--json) or terse (--quiet), when the user opted out via
// AUTOSK_AUTOINIT_ASSUME_YES, or when stdin/stderr are not real TTYs.
func shouldPromptForAutoInit() bool {
	if flagJSON || flagQuiet {
		return false
	}
	if os.Getenv(EnvAutoInitAssumeYes) != "" {
		return false
	}
	return isInteractiveFn()
}

// promptCreateDB renders the y/n prompt on stderr and reads one line
// of input. Empty / "y" / "yes" → true. "n" / "no" → false. Anything
// else loops with a short "answer y or n" hint. The bufio.Reader is
// created fresh per call so callers do not need to worry about
// residual buffering across re-prompts.
func promptCreateDB(cwd string) (bool, error) {
	fmt.Fprintf(os.Stderr, "No autosk database found at or above %s.\n", cwd)
	br := bufio.NewReader(confirmReader)
	for {
		fmt.Fprintf(os.Stderr, "Create a new autosk database in %s/.autosk/db? [Y/n] ", cwd)
		line, err := br.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return false, err
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "", "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		}
		fmt.Fprintln(os.Stderr, "please answer 'y' or 'n'")
		// On EOF without a recognised answer, do not loop forever:
		// treat it as a decline so a misconfigured non-TTY (which
		// shouldn't reach this prompt at all) cannot hang.
		if errors.Is(err, io.EOF) {
			return false, nil
		}
	}
}
