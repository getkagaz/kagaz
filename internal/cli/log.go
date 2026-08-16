package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/getkagaz/kagaz/internal/vaultkit/audit"
	"github.com/spf13/cobra"
)

// LogPayload is the `kagaz log --json` body.
type LogPayload struct {
	// Path is the audit log's absolute path.
	Path string `json:"path"`
	// Entries are the tail of the log, oldest first.
	Entries []audit.Entry `json:"entries"`
}

func newLogCommand(rt *Runtime) *cobra.Command {
	var n int
	cmd := &cobra.Command{
		Use:   "log",
		Short: "Print the tail of the audit log",
		Long: "log prints the append-only JSONL file every mutation and every confidential\n" +
			"resolution writes to. It is a plain file: grep, tail and jq all work on it.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg, err := rt.Config()
			if err != nil {
				return err
			}
			log := rt.Audit(cfg)
			entries, err := log.Tail(n)
			if err != nil {
				return err
			}
			if entries == nil {
				entries = []audit.Entry{}
			}
			return rt.Emit(&Response{
				Command: "log",
				Status:  StatusOK,
				Payload: LogPayload{Path: log.Path(), Entries: entries},
				Human:   humanLog,
			})
		},
	}
	cmd.Flags().IntVarP(&n, "lines", "n", 20, "how many entries to show")
	return cmd
}

func humanLog(w io.Writer, payload any) error {
	p, ok := payload.(LogPayload)
	if !ok {
		return fmt.Errorf("log: unexpected payload %T", payload)
	}
	if len(p.Entries) == 0 {
		fmt.Fprintf(w, "no audit entries yet (%s)\n", p.Path)
		return nil
	}
	for _, e := range p.Entries {
		fmt.Fprintf(w, "%s  %-12s", e.Time, e.Op)
		if e.Confirmed {
			fmt.Fprint(w, " confirmed")
		}
		if len(e.Paths) > 0 {
			fmt.Fprintf(w, " %s", strings.Join(e.Paths, ", "))
		}
		fmt.Fprintln(w)
		if e.Manifest != "" {
			fmt.Fprintf(w, "    manifest: %s\n", e.Manifest)
		}
		for _, k := range sortedKeys(e.Detail) {
			fmt.Fprintf(w, "    %s: %s\n", k, e.Detail[k])
		}
	}
	return nil
}
