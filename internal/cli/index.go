package cli

import (
	"fmt"
	"io"
	"path/filepath"

	"github.com/getkagaz/kagaz/internal/vaultkit/index"
	"github.com/spf13/cobra"
)

// IndexPayload is the `kagaz index --json` body.
type IndexPayload struct {
	// Written lists the absolute paths of the regenerated files.
	Written []string `json:"written"`
}

func newIndexCommand(rt *Runtime) *cobra.Command {
	return &cobra.Command{
		Use:   "index",
		Short: "Regenerate INDEX.md and AGENTS.md",
		Long: "index rewrites the vault's two generated documents: INDEX.md (what the vault\n" +
			"holds and how to query it) and AGENTS.md (the same vault explained to an agent).\n" +
			"Both carry a GENERATED banner and are overwritten on every run.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := rt.Config()
			if err != nil {
				return err
			}
			written, err := index.Generate(cmd.Context(), cfg)
			if err != nil {
				return err
			}
			if written == nil {
				written = []string{}
			}
			return rt.Emit(&Response{
				Command: "index",
				Status:  StatusOK,
				Payload: IndexPayload{Written: written},
				Human:   humanIndex,
			})
		},
	}
}

func humanIndex(w io.Writer, payload any) error {
	p, ok := payload.(IndexPayload)
	if !ok {
		return fmt.Errorf("index: unexpected payload %T", payload)
	}
	for _, f := range p.Written {
		fmt.Fprintf(w, "wrote %s\n", filepath.Base(f))
	}
	return nil
}
