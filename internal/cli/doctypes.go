package cli

import (
	"fmt"
	"io"
	"sort"

	"github.com/getkagaz/kagaz/internal/vaultkit/config"
	"github.com/getkagaz/kagaz/internal/vaultkit/search"
	"github.com/spf13/cobra"
)

// Doctype sources. There are exactly two, because Resolve merges exactly two
// inputs: the shipped catalog and the vault's own `doctypes:` block.
const (
	// SourceBuiltIn marks a doctype the CLI ships.
	SourceBuiltIn = "built-in"
	// SourceVault marks a doctype this vault defines, whether it adds a new
	// name or replaces a built-in of the same name. An override reports as
	// `vault` because what the catalog resolved to is the vault's entry, and
	// reporting the displaced built-in would name a definition nothing uses.
	SourceVault = "vault"
)

// DoctypeEntry is one resolved catalog entry as `kagaz doctypes --json`
// reports it.
type DoctypeEntry struct {
	Name string `json:"name"`
	// Category is whatever the catalog resolved, never a default and never
	// inferred from the name; absent when the catalog genuinely has none.
	Category string `json:"category,omitempty"`
	// Source is SourceBuiltIn or SourceVault.
	Source string `json:"source"`
	// Filed is how many documents in this vault currently carry this doctype,
	// counted the same way INDEX.md counts them: one walk, each document's own
	// best-known doctype. A doctype that exists but has never been filed
	// reports 0, which is a counted zero, not an absent count.
	Filed int `json:"filed"`
}

// DoctypeCounts is the arithmetic of the listing. Total always equals the
// length of the reported list, and vault + built_in always equals total: a
// consumer that renders a "N doctypes" header must not have to add up the
// list itself and hope it agrees.
type DoctypeCounts struct {
	Total   int `json:"total"`
	Vault   int `json:"vault"`
	BuiltIn int `json:"built_in"`
}

// DoctypesPayload is the `kagaz doctypes --json` body.
type DoctypesPayload struct {
	Doctypes []DoctypeEntry `json:"doctypes"`
	Counts   DoctypeCounts  `json:"counts"`
}

func newDoctypesCommand(rt *Runtime) *cobra.Command {
	return &cobra.Command{
		Use:   "doctypes",
		Short: "List the vault's resolved doctype catalog (read-only)",
		Long: "doctypes reports the catalog this vault actually resolves to: the built-in\n" +
			"document kinds whose category the vault's structure defines, plus every entry\n" +
			"from its own `doctypes:` block, with how many documents are filed under each.\n\n" +
			"It is read-only, like find: nothing is written, moved or tagged.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := rt.Config()
			if err != nil {
				return err
			}
			// search.New resolves the catalog itself, so the list reported here
			// and the set `find --doctype` filters against are one object. A
			// second doctypes.Resolve would be a second answer to the same
			// question, free to drift from the first.
			searcher, err := search.New(cfg)
			if err != nil {
				return err
			}
			tree, err := searcher.Scan(cmd.Context())
			if err != nil {
				return err
			}

			filed := map[string]int{}
			for i := range tree.Documents {
				filed[tree.Documents[i].DocType()]++
			}
			// A name in cfg.DocTypes is what `vault` means; the merge itself
			// stays in doctypes.Resolve. Slug because Resolve slugs, and a
			// vault that writes `Warranty Card` must still be recognised as
			// the definer of `warranty-card`.
			fromVault := map[string]bool{}
			for _, d := range cfg.DocTypes {
				fromVault[config.Slug(d.Name)] = true
			}

			payload := DoctypesPayload{Doctypes: []DoctypeEntry{}}
			for _, dt := range searcher.Catalog().All() {
				source := SourceBuiltIn
				if fromVault[dt.Name] {
					source = SourceVault
				}
				payload.Doctypes = append(payload.Doctypes, DoctypeEntry{
					Name:     dt.Name,
					Category: dt.Category,
					Source:   source,
					Filed:    filed[dt.Name],
				})
			}
			sort.Slice(payload.Doctypes, func(i, j int) bool {
				return payload.Doctypes[i].Name < payload.Doctypes[j].Name
			})
			for _, e := range payload.Doctypes {
				payload.Counts.Total++
				if e.Source == SourceVault {
					payload.Counts.Vault++
				} else {
					payload.Counts.BuiltIn++
				}
			}

			return rt.Emit(&Response{
				Command:  "doctypes",
				Status:   StatusOK,
				Payload:  payload,
				Warnings: tree.Warnings,
				Human:    humanDoctypes,
			})
		},
	}
}

func humanDoctypes(w io.Writer, payload any) error {
	p, ok := payload.(DoctypesPayload)
	if !ok {
		return fmt.Errorf("doctypes: unexpected payload %T", payload)
	}
	if len(p.Doctypes) == 0 {
		fmt.Fprintln(w, "no doctypes resolved")
		return nil
	}
	// Widths follow the data rather than a guess, so a long vault-defined name
	// pushes the columns out instead of wrapping the row.
	nameW, catW := len("DOCTYPE"), len("CATEGORY")
	for _, e := range p.Doctypes {
		if len(e.Name) > nameW {
			nameW = len(e.Name)
		}
		if len(e.Category) > catW {
			catW = len(e.Category)
		}
	}
	fmt.Fprintf(w, "%-*s  %-*s  %-8s  %5s\n", nameW, "DOCTYPE", catW, "CATEGORY", "SOURCE", "FILED")
	for _, e := range p.Doctypes {
		category := e.Category
		if category == "" {
			category = "—"
		}
		fmt.Fprintf(w, "%-*s  %-*s  %-8s  %5d\n", nameW, e.Name, catW, category, e.Source, e.Filed)
	}
	fmt.Fprintf(w, "\n%d doctype(s): %d built-in, %d vault-defined\n",
		p.Counts.Total, p.Counts.BuiltIn, p.Counts.Vault)
	return nil
}
