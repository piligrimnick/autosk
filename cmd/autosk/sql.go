package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// newSQLCmd: raw SQL passthrough. Read-only by default.
//
// Detection is conservative: any statement whose first significant keyword
// isn't SELECT/PRAGMA/EXPLAIN/WITH is treated as a write.
func newSQLCmd() *cobra.Command {
	var (
		write  bool
		pretty bool
		file   string
	)
	cmd := &cobra.Command{
		Use:   "sql [<query>]",
		Short: "Run a raw SQL query (read-only by default; --write to mutate)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			query, err := readQuery(args, file)
			if err != nil {
				return err
			}
			query = strings.TrimSpace(query)
			if query == "" {
				return errors.New("query is empty")
			}

			isWrite := !looksReadOnly(query)
			if isWrite && !write {
				return errors.New("statement appears to write; pass --write to allow")
			}

			if isWrite {
				cl, err := writeClient(cmd.Context())
				if err != nil {
					return err
				}
				n, err := cl.SQLExec(cmd.Context(), query)
				if err != nil {
					return err
				}
				if !flagQuiet {
					fmt.Printf("rows affected: %d\n", n)
				}
				return nil
			}

			cl, err := readClient(cmd.Context())
			if err != nil {
				return err
			}
			res, err := cl.SQLQuery(cmd.Context(), query)
			if err != nil {
				return err
			}
			cols := res.Columns
			data := res.Rows

			switch {
			case flagJSON:
				return emitJSON(cols, data)
			case pretty:
				return emitTable(cols, data)
			default:
				return emitTSV(cols, data)
			}
		},
	}
	cmd.Flags().BoolVar(&write, "write", false, "allow writes (without this, mutating SQL is refused)")
	cmd.Flags().BoolVar(&pretty, "pretty", false, "human-readable table output (default: TSV)")
	cmd.Flags().StringVar(&file, "file", "", "read query from a file (use '-' for stdin)")
	return cmd
}

func readQuery(args []string, file string) (string, error) {
	switch {
	case len(args) == 1 && file != "":
		return "", errors.New("provide a positional query OR --file, not both")
	case len(args) == 1:
		return args[0], nil
	case file == "-":
		b, err := os.ReadFile("/dev/stdin")
		return string(b), err
	case file != "":
		b, err := os.ReadFile(file)
		return string(b), err
	default:
		return "", errors.New("no query provided (positional arg or --file)")
	}
}

// looksReadOnly returns true if the statement's first significant keyword
// matches a known read-only verb. Conservative — false positives error out
// with a clear message.
func looksReadOnly(q string) bool {
	q = strings.ToUpper(strings.TrimLeft(q, " \t\r\n;"))
	switch {
	case strings.HasPrefix(q, "SELECT"),
		strings.HasPrefix(q, "PRAGMA"),
		strings.HasPrefix(q, "EXPLAIN"),
		strings.HasPrefix(q, "WITH"):
		return true
	}
	return false
}

func emitTSV(cols []string, rows [][]any) error {
	fmt.Println(strings.Join(cols, "\t"))
	for _, r := range rows {
		parts := make([]string, len(r))
		for i, v := range r {
			parts[i] = fmt.Sprintf("%v", normalize(v))
		}
		fmt.Println(strings.Join(parts, "\t"))
	}
	return nil
}

func emitTable(cols []string, rows [][]any) error {
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, strings.Join(cols, "\t"))
	for _, r := range rows {
		parts := make([]string, len(r))
		for i, v := range r {
			parts[i] = fmt.Sprintf("%v", normalize(v))
		}
		fmt.Fprintln(tw, strings.Join(parts, "\t"))
	}
	return tw.Flush()
}

func emitJSON(cols []string, rows [][]any) error {
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		m := make(map[string]any, len(cols))
		for i, c := range cols {
			m[c] = normalize(r[i])
		}
		out = append(out, m)
	}
	b, err := json.Marshal(out)
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(append(b, '\n'))
	return err
}

func normalize(v any) any {
	switch x := v.(type) {
	case []byte:
		return string(x)
	default:
		return v
	}
}
