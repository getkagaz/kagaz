package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/getkagaz/kagaz/internal/vaultkit/audit"
	"github.com/getkagaz/kagaz/internal/vaultkit/config"
	"github.com/getkagaz/kagaz/internal/vaultkit/conventions"
	"github.com/getkagaz/kagaz/internal/vaultkit/doctypes"
	"github.com/getkagaz/kagaz/internal/vaultkit/move"
	"github.com/getkagaz/kagaz/internal/vaultkit/sidecar"
	"github.com/spf13/cobra"
)

// MovePayload is the `kagaz move --json` body.
type MovePayload struct {
	// Moves are the relocations, proposed or performed.
	Moves []MoveRecord `json:"moves"`
	// Skipped are relocations that were not performed.
	Skipped []MoveRecord `json:"skipped,omitempty"`
	// Manifest is the manifest `kagaz rollback` reverses.
	Manifest string `json:"manifest,omitempty"`
	// Derived reports that the destination came from the vault's conventions
	// rather than from the command line.
	Derived bool `json:"derived"`
}

func newMoveCommand(rt *Runtime) *cobra.Command {
	var mut mutationFlags

	cmd := &cobra.Command{
		Use:   "move <path> [destination]",
		Short: "Move a document, or re-file it where the conventions say it belongs",
		Long: "move relocates a document through move.Engine: a SHA256-verified copy, tags\n" +
			"and sidecar carried across, and the source renamed into the staging folder\n" +
			"rather than deleted. With no destination, the conventional path is derived\n" +
			"from the document's own facts.",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(_ *cobra.Command, args []string) error {
			cfg, err := rt.Config()
			if err != nil {
				return err
			}
			src, err := documentPath(cfg, args[0])
			if err != nil {
				return err
			}

			payload := MovePayload{}
			var dst string
			if len(args) == 2 {
				dst, err = destinationPath(cfg, args[1], filepath.Base(src))
				if err != nil {
					return err
				}
			} else {
				dst, err = conventionalPath(cfg, src)
				if err != nil {
					return err
				}
				payload.Derived = true
			}
			if dst == src {
				payload.Moves = []MoveRecord{}
				payload.Skipped = []MoveRecord{{From: src, To: dst}}
				return rt.Emit(&Response{
					Command: "move", Status: StatusOK, Payload: payload, Human: humanMove,
					Warnings: []string{"the document is already at its conventional path; nothing to do"},
				})
			}
			payload.Moves = []MoveRecord{{From: src, To: dst}}

			approved, resp := mut.approve(rt, "move", payload, previewMove)
			if !approved {
				return rt.Emit(resp)
			}

			engine := move.New(cfg.ManifestDir(), cfg.StagingDir())
			res, err := engine.Execute("move", []move.Op{{Src: src, Dst: dst}})
			if err != nil {
				return err
			}
			payload.Moves = records(res.Moved)
			payload.Skipped = records(res.Skipped)
			if res.Manifest != nil {
				payload.Manifest = res.Manifest.Path
			}

			warnings := res.Warnings
			log := rt.Audit(cfg)
			if err := log.Append(audit.Entry{
				Op: "move", Paths: []string{dst}, Manifest: payload.Manifest,
				Detail: map[string]string{"from": src},
			}); err != nil {
				warnings = append(warnings, fmt.Sprintf("audit line not written: %v", err))
			}

			return rt.Emit(&Response{
				Command: "move", Status: StatusOK, Payload: payload,
				Warnings: warnings, Human: humanMove,
			})
		},
	}
	mut.register(cmd)
	return cmd
}

// destinationPath resolves a user-supplied destination: an existing directory
// means "into this folder under the source's own name".
func destinationPath(cfg *config.Config, arg, base string) (string, error) {
	abs := arg
	if !filepath.IsAbs(abs) {
		if _, err := os.Stat(abs); err != nil {
			abs = filepath.Join(cfg.VaultRoot, filepath.FromSlash(arg))
		}
	}
	abs, err := filepath.Abs(config.ExpandHome(abs))
	if err != nil {
		return "", err
	}
	if st, err := os.Stat(abs); err == nil && st.IsDir() {
		return filepath.Join(abs, base), nil
	}
	return abs, nil
}

// conventionalPath derives where a document belongs from its own facts: the
// filename when it parses, the sidecar otherwise. It refuses to guess — a move
// built on an invented fact is how a document becomes unfindable.
func conventionalPath(cfg *config.Config, src string) (string, error) {
	conv, err := conventions.New(cfg)
	if err != nil {
		return "", err
	}
	catalog, err := doctypes.Resolve(cfg)
	if err != nil {
		return "", err
	}

	var doc conventions.Doc
	if parsed, ok := conv.Parse(filepath.Base(src)); ok {
		doc = parsed
	} else if meta, err := sidecar.Read(src); err == nil && meta != nil {
		doc = conventions.Doc{
			DocType:    config.Slug(meta.DocType),
			Owners:     meta.Owners,
			Identifier: meta.Identifier,
			Year:       meta.Year,
			Ext:        filepath.Ext(src),
		}
	} else {
		return "", errors.New("this filename does not match the vault grammar and there is no sidecar to read its facts from; " +
			"give an explicit destination, or re-ingest it with `kagaz ingest --reindex`")
	}
	if doc.Ext == "" {
		doc.Ext = filepath.Ext(src)
	}
	cat, ok := catalog.CategoryOf(doc.DocType)
	if !ok {
		return "", fmt.Errorf("doctype %q is not in this vault's catalog, so its conventional folder is undefined", doc.DocType)
	}
	doc.Category = cat
	return conv.Path(doc)
}

func previewMove(w io.Writer, payload any) error {
	p, ok := payload.(MovePayload)
	if !ok {
		return fmt.Errorf("move: unexpected payload %T", payload)
	}
	for _, m := range p.Moves {
		fmt.Fprintf(w, "%s\n  -> %s\n", m.From, m.To)
	}
	if p.Derived {
		fmt.Fprintln(w, "(destination derived from the vault's conventions)")
	}
	return nil
}

func humanMove(w io.Writer, payload any) error {
	p, ok := payload.(MovePayload)
	if !ok {
		return fmt.Errorf("move: unexpected payload %T", payload)
	}
	for _, m := range p.Moves {
		fmt.Fprintf(w, "moved %s\n   -> %s\n", m.From, m.To)
	}
	for _, m := range p.Skipped {
		fmt.Fprintf(w, "skipped %s\n", m.From)
	}
	if p.Manifest != "" {
		fmt.Fprintf(w, "manifest: %s (reverse with `kagaz rollback %s`)\n", p.Manifest, filepath.Base(p.Manifest))
	}
	return nil
}
