package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
)

// newGCCmd wires `autosk gc` — an on-demand chunk-store compaction
// for the project DB resolved from cwd / --db / AUTOSK_DB.
//
// This is the operator-facing escape hatch when the daemon's
// scheduled compactor is disabled (--gc-interval<0) or when a long
// daemon-less burst of writes has bloated `.autosk/db` and made
// `autosk lazy` sluggish. The underlying SQL call is identical
// (`SELECT dolt_gc()`); see internal/store/doltlite/maint.go for the
// rationale.
//
// Output (default): "removed=<N> kept=<M> duration=<d>". --json
// dumps the full CompactResult including the verbatim doltlite
// reply string.
func newGCCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "gc",
		Short: "Run doltlite chunk-store garbage collection on the project DB",
		Long: "gc invokes SELECT dolt_gc() against the resolved .autosk/db.\n" +
			"It reclaims stale chunks left behind by previous writes so\n" +
			"`autosk lazy` and other readers don't have to replay an ever-\n" +
			"growing chunk-store WAL on every query.\n\n" +
			"Safe to run while the daemon is up: doltlite is single-writer\n" +
			"and the call serialises against any in-flight write.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			cl, err := readClient(ctx)
			if err != nil {
				return err
			}
			// The daemon does not time the call; measure wall-clock here so
			// the `duration=` field matches the pre-daemon CLI output.
			start := time.Now()
			g, err := cl.Compact(ctx)
			if err != nil {
				return cleanRPCError(err)
			}
			res := gcResult{
				ChunksRemoved: g.ChunksRemoved,
				ChunksKept:    g.ChunksKept,
				Raw:           g.Raw,
				Duration:      time.Since(start),
			}
			if flagJSON {
				return json.NewEncoder(os.Stdout).Encode(res)
			}
			if flagQuiet {
				return nil
			}
			fmt.Println(res.String())
			return nil
		},
	}
	return cmd
}

// gcResult mirrors the pre-daemon doltlite.CompactResult shape for the
// CLI's human (`removed=N kept=M duration=d`) + --json output. Duration
// is measured client-side around the maint.compact RPC.
type gcResult struct {
	ChunksRemoved int64
	ChunksKept    int64
	Raw           string
	Duration      time.Duration
}

func (r gcResult) String() string {
	return fmt.Sprintf("removed=%d kept=%d duration=%s",
		r.ChunksRemoved, r.ChunksKept, r.Duration.Round(time.Millisecond))
}
