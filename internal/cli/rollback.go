package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/getkagaz/kagaz/internal/vaultkit/audit"
	"github.com/getkagaz/kagaz/internal/vaultkit/move"
	"github.com/spf13/cobra"
)

// MoveRecord is one file relocation, as reported by every command that moves
// files. Sharing one shape across ingest, move, lint --fix and rollback is what
// lets a caller handle "what moved" once.
type MoveRecord struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// RollbackPayload is the `kagaz rollback --json` body.
type RollbackPayload struct {
	// Manifest is the manifest being reversed.
	Manifest string `json:"manifest"`
	// Op is the operation the manifest recorded.
	Op string `json:"op"`
	// Restored are the files put back where they came from.
	Restored []MoveRecord `json:"restored"`
	// Skipped are rows that could not be reversed and why they were left.
	Skipped []MoveRecord `json:"skipped,omitempty"`
}

func newRollbackCommand(rt *Runtime) *cobra.Command {
	var mut mutationFlags
	cmd := &cobra.Command{
		Use:   "rollback <manifest>",
		Short: "Reverse a manifest written by a previous mutation",
		Long: "rollback moves every file in a manifest from its post-operation path back to\n" +
			"its pre-operation path. It is safe to run twice: a row whose file is already\n" +
			"gone, or whose original path is occupied again, is reported and skipped.\n\n" +
			"With no argument it lists the manifests available in the vault.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			cfg, err := rt.Config()
			if err != nil {
				return err
			}
			if len(args) == 0 {
				return listManifests(rt, cfg.ManifestDir())
			}
			path := args[0]
			if !filepath.IsAbs(path) {
				if _, statErr := os.Stat(path); statErr != nil {
					path = filepath.Join(cfg.ManifestDir(), filepath.Base(path))
				}
			}
			man, err := move.ReadManifest(path)
			if err != nil {
				return err
			}

			payload := RollbackPayload{Manifest: man.Path, Op: man.Op, Restored: []MoveRecord{}}
			for _, row := range man.Rows {
				payload.Restored = append(payload.Restored, MoveRecord{From: row.CurrentPath, To: row.OriginalPath})
			}

			approved, resp := mut.approve(rt, "rollback", payload, previewRollback)
			if !approved {
				return rt.Emit(resp)
			}

			engine := move.New(cfg.ManifestDir(), cfg.StagingDir())
			res, err := engine.Rollback(man)
			if err != nil {
				return err
			}
			payload.Restored = records(res.Moved)
			payload.Skipped = records(res.Skipped)

			log := rt.Audit(cfg)
			paths := make([]string, 0, len(payload.Restored))
			for _, r := range payload.Restored {
				paths = append(paths, r.To)
			}
			if err := log.Append(audit.Entry{
				Op: "rollback", Paths: paths, Manifest: man.Path,
				Detail: map[string]string{"restored": fmt.Sprint(len(payload.Restored)), "skipped": fmt.Sprint(len(payload.Skipped))},
			}); err != nil {
				res.Warnings = append(res.Warnings, fmt.Sprintf("audit line not written: %v", err))
			}

			return rt.Emit(&Response{
				Command: "rollback", Status: StatusOK, Payload: payload,
				Warnings: res.Warnings, Human: humanRollback,
			})
		},
	}
	mut.register(cmd)
	return cmd
}

// records converts engine ops into the reported shape.
func records(ops []move.Op) []MoveRecord {
	out := make([]MoveRecord, 0, len(ops))
	for _, op := range ops {
		out = append(out, MoveRecord{From: op.Src, To: op.Dst})
	}
	return out
}

// ManifestListPayload is the body of `kagaz rollback` with no argument.
type ManifestListPayload struct {
	Dir       string   `json:"dir"`
	Manifests []string `json:"manifests"`
}

func listManifests(rt *Runtime, dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	payload := ManifestListPayload{Dir: dir, Manifests: []string{}}
	for _, e := range entries {
		if !e.IsDir() {
			payload.Manifests = append(payload.Manifests, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(payload.Manifests)
	return rt.Emit(&Response{
		Command: "rollback", Status: StatusOK, Payload: payload,
		Human: func(w io.Writer, p any) error {
			list, ok := p.(ManifestListPayload)
			if !ok {
				return fmt.Errorf("rollback: unexpected payload %T", p)
			}
			if len(list.Manifests) == 0 {
				fmt.Fprintf(w, "no manifests in %s\n", list.Dir)
				return nil
			}
			fmt.Fprintf(w, "manifests in %s:\n", list.Dir)
			for _, m := range list.Manifests {
				fmt.Fprintf(w, "  %s\n", filepath.Base(m))
			}
			fmt.Fprintln(w, "\nreverse one with `kagaz rollback <manifest>`")
			return nil
		},
	})
}

func previewRollback(w io.Writer, payload any) error {
	p, ok := payload.(RollbackPayload)
	if !ok {
		return fmt.Errorf("rollback: unexpected payload %T", payload)
	}
	fmt.Fprintf(w, "reverse %s (%s), %d file(s):\n", filepath.Base(p.Manifest), p.Op, len(p.Restored))
	for _, r := range p.Restored {
		fmt.Fprintf(w, "  %s\n    -> %s\n", r.From, r.To)
	}
	return nil
}

func humanRollback(w io.Writer, payload any) error {
	p, ok := payload.(RollbackPayload)
	if !ok {
		return fmt.Errorf("rollback: unexpected payload %T", payload)
	}
	for _, r := range p.Restored {
		fmt.Fprintf(w, "restored %s\n", r.To)
	}
	for _, r := range p.Skipped {
		fmt.Fprintf(w, "skipped  %s\n", r.From)
	}
	fmt.Fprintf(w, "\n%d restored, %d skipped\n", len(p.Restored), len(p.Skipped))
	return nil
}
