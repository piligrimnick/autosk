package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"autosk/internal/store/doltlite"
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
			s, closeFn, err := openStore(ctx, false)
			if err != nil {
				return err
			}
			defer closeFn()
			dl, ok := s.(*doltlite.Store)
			if !ok {
				return fmt.Errorf("gc: store is not doltlite (got %T); compaction is doltlite-specific", s)
			}
			res, err := dl.Compact(ctx)
			if err != nil {
				return err
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
