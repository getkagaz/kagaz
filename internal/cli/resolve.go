package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/getkagaz/kagaz/internal/vaultkit/audit"
	"github.com/getkagaz/kagaz/internal/vaultkit/search"
	"github.com/spf13/cobra"
)

// ResolvePayload is the `kagaz resolve --json` body.
//
// Two path fields, on purpose:
//
//   - Path is the document's vault-relative path. It names *which* document is
//     being talked about and is safe to show in a refusal, which is exactly how
//     docs/commands.md renders the confirmation_required response.
//   - ResolvedPath is the absolute, materialized, ready-to-hand-to-another-tool
//     path. It is populated only when the resolution actually succeeded — never
//     on a confirmation_required response, and never when materialization
//     failed. That is the whole point of the confidential gate: a caller that
//     receives ResolvedPath knows the bytes are on this machine and that
//     consent was recorded.
type ResolvePayload struct {
	Path         string   `json:"path"`
	ResolvedPath string   `json:"resolved_path,omitempty"`
	DocType      string   `json:"doctype,omitempty"`
	Tags         []string `json:"tags,omitempty"`
	ForSend      bool     `json:"for_send"`
	Gated        bool     `json:"gated"`
	Confirmed    bool     `json:"confirmed"`
	Materialized bool     `json:"materialized"`
	Reason       string   `json:"reason,omitempty"`
	Message      string   `json:"message,omitempty"`
}

func newResolveCommand(rt *Runtime) *cobra.Command {
	var (
		q       FindQuery
		forSend bool
		confirm bool
	)

	cmd := &cobra.Command{
		Use:   "resolve [reference]",
		Short: "Resolve a document reference to its path on this machine",
		Long: "resolve turns a path or a filter expression into the document's current\n" +
			"on-disk path, downloading it from iCloud first when it is an evicted\n" +
			"placeholder. It fails rather than returning a path whose bytes are not here.\n\n" +
			"--for-send is the confidential gate: resolving a gated document for handoff\n" +
			"outside the vault requires explicit confirmation and always writes an audit\n" +
			"line, confirmed or refused. With --json it never prompts; it returns a\n" +
			"confirmation_required response and a non-zero exit, and --confirm supplies\n" +
			"the consent a prompt would have gathered.",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := rt.Config()
			if err != nil {
				return err
			}
			q.Text = strings.TrimSpace(strings.Join(args, " "))

			doc, err := resolveOne(cmd.Context(), rt, q)
			if err != nil {
				return err
			}

			payload := ResolvePayload{
				Path:    doc.RelPath,
				DocType: doc.DocType(),
				Tags:    doc.Tags,
				ForSend: forSend,
			}
			if payload.Tags == nil {
				payload.Tags = []string{}
			}

			if !forSend {
				if err := materialize(cmd.Context(), doc.Path); err != nil {
					return err
				}
				payload.ResolvedPath = doc.Path
				payload.Materialized = true
				return rt.Emit(&Response{Command: "resolve", Status: StatusOK, Payload: payload, Human: humanResolve})
			}

			gated, reason := gatedForSend(rt, doc)
			payload.Gated = gated
			payload.Reason = reason

			// Consent, in the order the ruling requires: an explicit --confirm
			// is consent; otherwise a terminal may be asked; a --json caller is
			// never asked and never silently approved.
			approved := !gated || confirm
			if gated && !confirm && !rt.JSON && rt.Interactive() {
				fmt.Fprintf(rt.Out, "%s is %s.\nResolving it for external send will be recorded in the audit log.\n",
					doc.RelPath, reason)
				ok, cerr := rt.Confirm("Resolve it for send?")
				if cerr != nil {
					return cerr
				}
				approved = ok
			}

			log := rt.Audit(cfg)
			entry := audit.Entry{
				Op:        "resolve-for-send",
				Paths:     []string{doc.Path},
				Confirmed: approved,
				Detail: map[string]string{
					"gated":  fmt.Sprint(gated),
					"reason": reason,
				},
			}
			if !approved {
				entry.Detail["outcome"] = "not confirmed"
			}
			var warnings []string
			// The audit line is written before the path is handed over, and on
			// the refusal path too. There is no mode that skips it.
			if err := log.Append(entry); err != nil {
				return fmt.Errorf("refusing to resolve for send: the audit line could not be written: %w", err)
			}

			if !approved {
				payload.Message = "re-run with --confirm to resolve this document for external send"
				return rt.Emit(&Response{
					Command:  "resolve",
					Status:   StatusConfirmationRequired,
					Payload:  payload,
					Human:    humanResolve,
					Warnings: warnings,
					Exit:     ExitConfirmationRequired,
				})
			}

			if err := materialize(cmd.Context(), doc.Path); err != nil {
				return err
			}
			payload.Confirmed = true
			payload.Materialized = true
			payload.ResolvedPath = doc.Path
			return rt.Emit(&Response{
				Command: "resolve", Status: StatusOK, Payload: payload,
				Warnings: warnings, Human: humanResolve,
			})
		},
	}

	f := cmd.Flags()
	f.BoolVar(&forSend, "for-send", false, "resolve for handoff outside the vault (confidential gate)")
	f.BoolVar(&confirm, "confirm", false, "supply the explicit confirmation --for-send requires")
	f.StringVar(&q.Person, "person", "", "person display name or tag")
	f.StringVar(&q.Company, "company", "", "company tag")
	f.StringVar(&q.Area, "area", "", "area tag")
	f.StringVar(&q.DocType, "doctype", "", "catalog doctype name")
	f.StringSliceVar(&q.Tags, "tag", nil, "Finder tag that must be present (repeatable)")
	f.BoolVar(&q.Active, "active", false, "only documents tagged active")
	f.StringVar(&q.Period, "period", "", "calendar or fiscal period")
	return cmd
}

// materialize brings an iCloud-evicted document's bytes onto this machine.
//
// A failure here is fatal by design (safety invariant 7 and the resolve
// contract): returning a placeholder path would hand the caller a filename
// whose bytes are not there, and every caller of resolve is about to read it.
func materialize(ctx context.Context, path string) error {
	if err := search.Materialize(ctx, path); err != nil {
		if errors.Is(err, search.ErrNoBrctl) {
			return fmt.Errorf("%s is an iCloud placeholder and brctl is not available to download it: %w", path, err)
		}
		return fmt.Errorf("%s could not be made available on this machine: %w", path, err)
	}
	return nil
}

// gatedForSend reports whether this document needs explicit confirmation, and
// why. A vault that has turned the gate off is honoured, but the default when
// the key is absent is to gate (config.Confidential.ConfirmationRequired fails
// closed).
func gatedForSend(rt *Runtime, doc *search.Document) (bool, string) {
	cfg, err := rt.Config()
	if err != nil {
		return true, "the vault configuration could not be read"
	}
	if !cfg.Confidence.ConfirmationRequired() {
		return false, ""
	}
	if doc.HasTag("confidential") {
		return true, "tagged confidential"
	}
	if doc.TagsUnsupported {
		// Tags cannot be read here, so "not confidential" is unprovable. The
		// gate fails closed rather than assuming the safe answer.
		return true, "Finder tags are unreadable on this filesystem, so the confidential tag cannot be ruled out"
	}
	return false, ""
}

// resolveOne turns a reference into exactly one document. A reference that
// matches several documents is an error naming them: silently picking one is
// how the wrong document gets emailed.
func resolveOne(ctx context.Context, rt *Runtime, q FindQuery) (*search.Document, error) {
	cfg, err := rt.Config()
	if err != nil {
		return nil, err
	}
	searcher, err := search.New(cfg)
	if err != nil {
		return nil, err
	}

	// A reference that names an existing file is that file, with no search at
	// all — the common case, and the one an agent chaining `find` into
	// `resolve` produces.
	if q.Text != "" {
		if abs, ok := existingFile(q.Text); ok {
			tree, terr := searcher.Scan(ctx)
			if terr == nil {
				for i := range tree.Documents {
					if tree.Documents[i].Path == abs {
						return &tree.Documents[i], nil
					}
				}
			}
			return nil, fmt.Errorf("%s is not a document in this vault (%s)", abs, cfg.VaultRoot)
		}
	}

	searcher.Spotlight = search.NewMDFind()
	docs, err := searcher.Find(ctx, search.Query{
		Text: q.Text, Person: q.Person, Company: q.Company, Area: q.Area,
		DocType: q.DocType, Tags: q.Tags, Active: q.Active, Period: q.Period,
	})
	if err != nil {
		return nil, err
	}
	switch len(docs) {
	case 0:
		return nil, errors.New("no document matched that reference")
	case 1:
		return &docs[0], nil
	default:
		names := make([]string, 0, len(docs))
		for i := range docs {
			names = append(names, docs[i].RelPath)
		}
		if len(names) > 8 {
			names = append(names[:8], fmt.Sprintf("… and %d more", len(docs)-8))
		}
		return nil, fmt.Errorf("that reference matches %d documents; narrow it:\n  %s",
			len(docs), strings.Join(names, "\n  "))
	}
}

// existingFile reports whether ref names a regular file on disk.
func existingFile(ref string) (string, bool) {
	abs, err := filepath.Abs(ref)
	if err != nil {
		return "", false
	}
	if st, err := os.Stat(abs); err == nil && !st.IsDir() {
		return abs, true
	}
	// An evicted document has no directory entry of its own; its placeholder
	// stands in for it, and resolve exists precisely to bring it back.
	if _, err := os.Stat(search.PlaceholderPath(abs)); err == nil {
		return abs, true
	}
	return "", false
}

func humanResolve(w io.Writer, payload any) error {
	p, ok := payload.(ResolvePayload)
	if !ok {
		return fmt.Errorf("resolve: unexpected payload %T", payload)
	}
	if p.ResolvedPath != "" {
		fmt.Fprintln(w, p.ResolvedPath)
		return nil
	}
	fmt.Fprintf(w, "%s: confirmation required (%s)\n", p.Path, p.Reason)
	if p.Message != "" {
		fmt.Fprintf(w, "%s\n", p.Message)
	}
	return nil
}
