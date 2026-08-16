package cli

import (
	"fmt"
	"io"
	"sort"

	"github.com/getkagaz/kagaz/internal/vaultkit/lint"
	"github.com/spf13/cobra"
)

// LintRule is one rule as `kagaz lint --list-rules --json` reports it.
type LintRule struct {
	ID          string `json:"id"`
	Severity    string `json:"severity"`
	Fixable     bool   `json:"fixable"`
	Description string `json:"description"`
}

// LintPayload is the `kagaz lint --json` body.
type LintPayload struct {
	// Findings are every violation, sorted by path then rule id.
	Findings []lint.Finding `json:"findings"`
	// Counts is the finding count per severity, always with all three keys so
	// a consumer never has to distinguish absent from zero.
	Counts map[string]int `json:"counts"`
	// Fixable is how many findings --fix could repair.
	Fixable int `json:"fixable"`
	// Fixed and SkippedFixes are populated by --fix.
	Fixed        []lint.Finding `json:"fixed,omitempty"`
	SkippedFixes []lint.Finding `json:"skipped_fixes,omitempty"`
	// Manifest is the move manifest covering the repairs, when any file moved.
	Manifest string `json:"manifest,omitempty"`
	// Rules is populated by --list-rules instead of Findings.
	Rules []LintRule `json:"rules,omitempty"`
}

func newLintCommand(rt *Runtime) *cobra.Command {
	var (
		fix       bool
		listRules bool
		mut       mutationFlags
	)

	cmd := &cobra.Command{
		Use:   "lint",
		Short: "Check the vault against its own conventions",
		Long: "lint reports filename, folder, tag, lifecycle and sidecar violations. A finding\n" +
			"is a report, not a failure; --fix applies only the provably safe repairs, each\n" +
			"through move.Engine with its own manifest.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if listRules {
				payload := LintPayload{Counts: map[string]int{}}
				for _, r := range lint.Rules() {
					payload.Rules = append(payload.Rules, LintRule{
						ID: r.ID, Severity: string(r.Severity), Fixable: r.Fixable, Description: r.Description,
					})
				}
				return rt.Emit(&Response{Command: "lint", Status: StatusOK, Payload: payload, Human: humanLint})
			}

			cfg, err := rt.Config()
			if err != nil {
				return err
			}
			linter, err := lint.New(cfg)
			if err != nil {
				return err
			}
			findings, err := linter.Run(cmd.Context())
			if err != nil {
				return err
			}

			payload := LintPayload{Findings: findings, Counts: map[string]int{
				string(lint.SeverityError): 0, string(lint.SeverityWarning): 0, string(lint.SeverityInfo): 0,
			}}
			if payload.Findings == nil {
				payload.Findings = []lint.Finding{}
			}
			for _, f := range findings {
				payload.Counts[string(f.Severity)]++
				if f.Fixable {
					payload.Fixable++
				}
			}

			status := StatusOK
			exit := ExitOK
			if len(findings) > 0 {
				status = StatusFindings
			}
			if payload.Counts[string(lint.SeverityError)] > 0 {
				exit = ExitFindings
			}

			if !fix {
				return rt.Emit(&Response{Command: "lint", Status: status, Payload: payload, Human: humanLint, Exit: exit})
			}

			// --fix is a mutation like any other: preview, then confirm.
			fixable := make([]lint.Finding, 0, payload.Fixable)
			for _, f := range findings {
				if f.Fixable {
					fixable = append(fixable, f)
				}
			}
			if len(fixable) == 0 {
				return rt.Emit(&Response{Command: "lint", Status: status, Payload: payload, Human: humanLint, Exit: exit})
			}
			approved, resp := mut.approve(rt, "lint", payload, previewLintFixes)
			if !approved {
				return rt.Emit(resp)
			}

			res, err := linter.Fix(fixable)
			if err != nil {
				return err
			}
			payload.Fixed = res.Fixed
			payload.SkippedFixes = res.Skipped
			if res.Manifest != nil {
				payload.Manifest = res.Manifest.Path
			}
			return rt.Emit(&Response{
				Command: "lint", Status: StatusOK, Payload: payload,
				Warnings: res.Warnings, Human: humanLint,
			})
		},
	}

	cmd.Flags().BoolVar(&fix, "fix", false, "apply the provably safe repairs")
	cmd.Flags().BoolVar(&listRules, "list-rules", false, "list every rule and exit")
	mut.register(cmd)
	return cmd
}

// previewLintFixes renders the findings and, beneath them, exactly the repairs
// --fix would apply. It reads only the payload, so the preview a user approves
// and the proposal a JSON caller receives are the same description.
func previewLintFixes(w io.Writer, payload any) error {
	if err := humanLint(w, payload); err != nil {
		return err
	}
	p, ok := payload.(LintPayload)
	if !ok {
		return fmt.Errorf("lint: unexpected payload %T", payload)
	}
	fmt.Fprintf(w, "\n%d finding(s) can be repaired automatically:\n", p.Fixable)
	for _, f := range p.Findings {
		if f.Fixable {
			fmt.Fprintf(w, "  %s  %s\n", f.Path, repairText(f))
		}
	}
	return nil
}

// repairText describes a repair in one phrase.
func repairText(f lint.Finding) string {
	switch {
	case f.Repair == nil:
		return "(no repair)"
	case f.Repair.MoveTo != "":
		return "-> " + f.Repair.MoveTo
	case f.Repair.AddTag != "":
		return "+tag " + f.Repair.AddTag
	default:
		return "(no repair)"
	}
}

func humanLint(w io.Writer, payload any) error {
	p, ok := payload.(LintPayload)
	if !ok {
		return fmt.Errorf("lint: unexpected payload %T", payload)
	}
	if len(p.Rules) > 0 {
		for _, r := range p.Rules {
			fixable := ""
			if r.Fixable {
				fixable = " [fixable]"
			}
			fmt.Fprintf(w, "%-24s %-8s%s\n    %s\n", r.ID, r.Severity, fixable, r.Description)
		}
		return nil
	}
	if len(p.Findings) == 0 {
		fmt.Fprintln(w, "clean: no findings")
		return nil
	}
	for _, f := range p.Findings {
		fmt.Fprintf(w, "%s: %s [%s]\n    %s\n", f.Severity, f.Path, f.Rule, f.Message)
	}
	keys := make([]string, 0, len(p.Counts))
	for k := range p.Counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Fprintf(w, "\n%d finding(s):", len(p.Findings))
	for _, k := range keys {
		fmt.Fprintf(w, " %s=%d", k, p.Counts[k])
	}
	fmt.Fprintf(w, " (%d fixable)\n", p.Fixable)
	if len(p.Fixed) > 0 {
		fmt.Fprintf(w, "repaired %d finding(s)\n", len(p.Fixed))
	}
	if p.Manifest != "" {
		fmt.Fprintf(w, "manifest: %s (reverse with `kagaz rollback %s`)\n", p.Manifest, p.Manifest)
	}
	return nil
}
