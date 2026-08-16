package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/getkagaz/kagaz/internal/vaultkit/config"
	"github.com/spf13/cobra"
)

// InitPayload is the `kagaz init --json` body.
type InitPayload struct {
	// Vault is the absolute path of the vault.yaml that was written. It is
	// what `--vault` expects back.
	Vault string `json:"vault"`
	// Root is the vault root directory.
	Root string `json:"root"`
	// Created lists the directories created, relative to the root.
	Created []string `json:"created"`
	// Demo reports whether the vault was populated with demo documents.
	Demo bool `json:"demo"`
	// Documents is the number of demo documents written.
	Documents int `json:"documents"`
	// FiscalYearStart is the configured fiscal-year start month.
	FiscalYearStart int `json:"fiscal_year_start_month"`
	// Existing reports that vault.yaml was already there and was left alone.
	Existing bool `json:"existing"`
}

func newInitCommand(rt *Runtime) *cobra.Command {
	var (
		root    string
		fyStart int
		demo    bool
		force   bool
	)

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create a vault",
		Long: "init writes vault.yaml and the category folders it names. With --demo it also\n" +
			"fills the vault with synthetic documents, sidecars and Finder tags, so that\n" +
			"`kagaz find` returns results and `kagaz lint` is clean straight away.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if fyStart < 1 || fyStart > 12 {
				return fmt.Errorf("--fy-start must be 1-12, got %d", fyStart)
			}
			if root == "" {
				root = "~/Documents"
			}
			abs, err := filepath.Abs(config.ExpandHome(root))
			if err != nil {
				return err
			}
			vaultFile := filepath.Join(abs, config.FileName)

			payload := InitPayload{Vault: vaultFile, Root: abs, Demo: demo, FiscalYearStart: fyStart, Created: []string{}}

			if _, err := os.Stat(vaultFile); err == nil && !force {
				payload.Existing = true
				return rt.Emit(&Response{
					Command: "init",
					Status:  StatusOK,
					Payload: payload,
					Human:   humanInit,
					Warnings: []string{fmt.Sprintf("%s already exists; nothing was overwritten (use --force to rewrite it)",
						vaultFile)},
				})
			}

			if err := os.MkdirAll(abs, 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(vaultFile, []byte(vaultYAML(fyStart, demo)), 0o644); err != nil {
				return err
			}
			cfg, err := config.LoadFile(vaultFile)
			if err != nil {
				return fmt.Errorf("the vault.yaml just written does not load: %w", err)
			}
			rt.SetConfig(cfg)

			dirs := make([]string, 0, len(cfg.Structure))
			for _, cat := range cfg.Structure {
				dirs = append(dirs, cat.Path)
			}
			sort.Strings(dirs)
			for _, d := range dirs {
				if err := os.MkdirAll(filepath.Join(cfg.VaultRoot, filepath.FromSlash(d)), 0o755); err != nil {
					return err
				}
				payload.Created = append(payload.Created, d)
			}

			var warnings []string
			if demo {
				n, warn, err := writeDemoVault(cmd.Context(), cfg)
				if err != nil {
					return fmt.Errorf("populate demo vault: %w", err)
				}
				payload.Documents = n
				warnings = append(warnings, warn...)
			}

			return rt.Emit(&Response{
				Command:  "init",
				Status:   StatusOK,
				Payload:  payload,
				Warnings: warnings,
				Human:    humanInit,
			})
		},
	}

	f := cmd.Flags()
	f.StringVar(&root, "root", "", "vault root directory (default ~/Documents)")
	f.IntVar(&fyStart, "fy-start", 1, "fiscal year start month, 1-12")
	f.BoolVar(&demo, "demo", false, "populate an explorable demo vault")
	f.BoolVar(&force, "force", false, "overwrite an existing vault.yaml")
	return cmd
}

func humanInit(w io.Writer, payload any) error {
	p, ok := payload.(InitPayload)
	if !ok {
		return fmt.Errorf("init: unexpected payload %T", payload)
	}
	if p.Existing {
		fmt.Fprintf(w, "vault already configured: %s\n", p.Vault)
		return nil
	}
	fmt.Fprintf(w, "wrote %s\n", p.Vault)
	fmt.Fprintf(w, "created %d category folders under %s\n", len(p.Created), p.Root)
	if p.Demo {
		fmt.Fprintf(w, "populated %d demo documents with sidecars and Finder tags\n", p.Documents)
		fmt.Fprintf(w, "\nTry:\n  kagaz --vault %s find\n  kagaz --vault %s lint\n  kagaz --vault %s doctor\n",
			p.Vault, p.Vault, p.Vault)
	} else {
		fmt.Fprintf(w, "\nNext:\n  kagaz doctor\n  kagaz ingest <file>\n")
	}
	return nil
}

// vaultYAML renders the vault.yaml `init` writes. It is deliberately a short,
// commented file rather than every knob in examples/vault.yaml: config.go
// defaults everything omitted here, and a config full of values a user did not
// choose is a config nobody dares edit.
//
// With --demo the tag vocabulary is seeded to match the documents that are
// about to be written, including the fiscal-year tags fycal.Year.Tag() renders,
// so `kagaz lint` is clean on a freshly initialised demo vault.
func vaultYAML(fyStart int, demo bool) string {
	var b strings.Builder
	b.WriteString("# Kagaz vault configuration. Every field has a default (docs/configuration.md);\n")
	b.WriteString("# examples/vault.yaml in the Kagaz repository shows every available knob.\n")
	b.WriteString("version: 1\n\n")
	b.WriteString("# The vault root, relative to this file, so the vault stays portable.\n")
	b.WriteString("vault_root: .\n\n")
	fmt.Fprintf(&b, "fiscal_year:\n  start_month: %d\n", fyStart)
	if fyStart == 1 {
		b.WriteString("  label_format: \"FY {yyyy1}\"\n\n")
	} else {
		b.WriteString("  label_format: \"FY {yy1}-{yy2}\"\n\n")
	}

	b.WriteString("# The people this vault files documents for. `tag` defaults to a slug of `name`.\n")
	b.WriteString("people:\n")
	for _, p := range demoPeople {
		fmt.Fprintf(&b, "  - name: %s\n    tag: %s\n", p.Name, p.Tag)
	}
	b.WriteString("\n")

	b.WriteString("# The controlled Finder-tag vocabulary. A tag outside it is a `kagaz lint`\n")
	b.WriteString("# finding: an uncontrolled vocabulary makes saved searches unreliable.\n")
	b.WriteString("tags:\n")
	writeList := func(key string, values []string) {
		if len(values) == 0 {
			fmt.Fprintf(&b, "  %s: []\n", key)
			return
		}
		fmt.Fprintf(&b, "  %s:\n", key)
		for _, v := range values {
			fmt.Fprintf(&b, "    - %s\n", v)
		}
	}
	if demo {
		writeList("companies", demoCompanyTags())
		writeList("areas", demoAreaTags())
		writeList("fiscal_years", demoFiscalYearTags(fyStart))
	} else {
		writeList("companies", nil)
		writeList("areas", nil)
		writeList("fiscal_years", nil)
	}
	b.WriteString("\n")

	b.WriteString("lint:\n")
	b.WriteString("  require_lifecycle_tag: false\n")
	b.WriteString("  single_active_per_doctype_per_person:\n")
	b.WriteString("    - passport\n    - drivers-license\n    - insurance-policy\n")
	b.WriteString("  forbid_passwords_in_filenames: true\n\n")

	b.WriteString("confidential:\n")
	b.WriteString("  # Gates `kagaz resolve --for-send`: explicit confirmation, always audited.\n")
	b.WriteString("  require_confirmation_on_resolve_for_send: true\n")
	b.WriteString("  audit_log: vault.log\n")

	if demo {
		b.WriteString("\n# Per-vault additions to the built-in doctype catalog. This one shows the\n")
		b.WriteString("# mechanism: a document kind Kagaz does not ship, matched by its own keywords.\n")
		b.WriteString("doctypes:\n")
		b.WriteString("  - name: warranty-card\n")
		b.WriteString("    category: personal\n")
		b.WriteString("    match:\n")
		b.WriteString("      keywords:\n")
		b.WriteString("        - warranty card\n")
		b.WriteString("        - warranty period\n")
		b.WriteString("        - proof of purchase\n")
		b.WriteString("    extract:\n")
		b.WriteString("      purchase_date: '(?i)purchase\\s+date[:\\s]{1,4}([0-9]{1,2}[ /.\\-][A-Za-z0-9]{2,9}[ /.\\-][0-9]{2,4})'\n")
	}
	return b.String()
}
