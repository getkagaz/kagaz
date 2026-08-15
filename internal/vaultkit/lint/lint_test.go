package lint

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/getkagaz/kagaz/internal/vaultkit/config"
	"github.com/getkagaz/kagaz/internal/vaultkit/move"
	"github.com/getkagaz/kagaz/internal/vaultkit/tags"
)

const fixtureVault = "../../../testdata/fixture-vault/vault.yaml"

func fixtureLinter(t *testing.T) *Linter {
	t.Helper()
	abs, err := filepath.Abs(fixtureVault)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadFile(abs)
	if err != nil {
		t.Fatal(err)
	}
	l, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

type ruleAt struct {
	rule string
	path string
}

func summarize(findings []Finding) []ruleAt {
	out := make([]ruleAt, 0, len(findings))
	for _, f := range findings {
		out = append(out, ruleAt{f.Rule, f.Path})
	}
	return out
}

// TestFixtureVaultFindings is the contract stated in
// testdata/fixture-vault/README.md: exactly three findings, and none of them
// against the four documents that are correctly named and correctly placed. A
// new rule that fires on one of those four is a regression in the rule, not in
// the fixture.
func TestFixtureVaultFindings(t *testing.T) {
	l := fixtureLinter(t)
	findings, err := l.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []ruleAt{
		{RuleNameGrammar, "Financial/Alex-Rao/FY 2026/old invoice notes.txt"},
		{RuleStaleSidecar, "Financial/Sam-Rao/FY 2025/Receipt_Sam-Rao_Globex_2025.txt"},
		{RuleOrphanSidecar, "Travel/Sam-Rao/.Ticket_Sam-Rao_Delta-Airlines.txt.meta.yaml"},
	}
	if got := summarize(findings); !reflect.DeepEqual(got, want) {
		t.Errorf("findings:\n got %+v\nwant %+v", got, want)
	}
	for _, f := range findings {
		if f.Fixable != (f.Repair != nil) {
			t.Errorf("%s: Fixable=%v but Repair=%v", f.Rule, f.Fixable, f.Repair)
		}
		if f.Message == "" || f.Severity == "" {
			t.Errorf("%s: incomplete finding %+v", f.Rule, f)
		}
		if filepath.IsAbs(f.Path) {
			t.Errorf("%s: Path must be relative to the vault root, got %s", f.Rule, f.Path)
		}
	}
	// The malformed fixture filename has no sidecar to rebuild a name from, so
	// it must stay a permanent finding rather than become a guess.
	if findings[0].Fixable {
		t.Error("`old invoice notes.txt` must not be auto-fixable: nothing records what it is")
	}
}

func TestRunIsStable(t *testing.T) {
	l := fixtureLinter(t)
	first, err := l.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := l.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Error("two runs over an unchanged tree produced different findings")
	}
}

func TestRulesTableIsComplete(t *testing.T) {
	ids := map[string]bool{}
	for _, r := range Rules() {
		if ids[r.ID] {
			t.Errorf("duplicate rule id %q", r.ID)
		}
		ids[r.ID] = true
		if r.Description == "" || r.Severity == "" {
			t.Errorf("%s: incomplete rule documentation", r.ID)
		}
	}
	for _, id := range []string{
		RuleNameGrammar, RuleNameNormalization, RuleUnknownDocType, RuleWrongFolder,
		RuleUnknownTag, RuleMissingLifecycleTag, RuleMultipleActive,
		RulePasswordInFilename, RuleStaleSidecar, RuleOrphanSidecar,
	} {
		if !ids[id] {
			t.Errorf("rule %q is not in Rules()", id)
		}
	}
	sorted := sort.SliceIsSorted(Rules(), func(i, j int) bool { return Rules()[i].ID < Rules()[j].ID })
	if !sorted {
		t.Error("Rules() must be id-sorted")
	}
}

// vaultBuilder writes a throwaway vault so each rule can be provoked in
// isolation, with tags injected (they cannot be committed to git).
type vaultBuilder struct {
	t    *testing.T
	cfg  *config.Config
	tags map[string][]string
}

func newVault(t *testing.T, extraYAML string) *vaultBuilder {
	t.Helper()
	dir := t.TempDir()
	yaml := "version: 1\nvault_root: .\n" +
		"people:\n  - name: Alex Rao\n    tag: alex-rao\n  - name: Sam Rao\n    tag: sam-rao\n" +
		"tags:\n  companies:\n    - acme-corp\n  areas:\n    - tax\n  fiscal_years:\n    - fy2026\n" +
		"structure:\n  financial:\n    path: Financial\n    layout: \"{Owner}/{FY}\"\n" +
		"  travel:\n    path: Travel\n    layout: \"{Owner}\"\n" +
		"  identity:\n    path: Identity\n    shared: _Shared\n    layout: \"{Owner}\"\n" +
		extraYAML
	path := filepath.Join(dir, "vault.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return &vaultBuilder{t: t, cfg: cfg, tags: map[string][]string{}}
}

func (v *vaultBuilder) write(rel, content string) string {
	v.t.Helper()
	p := filepath.Join(v.cfg.VaultRoot, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		v.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		v.t.Fatal(err)
	}
	return p
}

func (v *vaultBuilder) tag(rel string, list ...string) {
	v.tags[rel] = list
}

func (v *vaultBuilder) linter() *Linter {
	v.t.Helper()
	l, err := New(v.cfg)
	if err != nil {
		v.t.Fatal(err)
	}
	root := v.cfg.VaultRoot
	captured := v.tags
	l.Search.ReadTags = func(path string) ([]string, error) {
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil, nil
		}
		return captured[filepath.ToSlash(rel)], nil
	}
	return l
}

func (v *vaultBuilder) run() []Finding {
	v.t.Helper()
	findings, err := v.linter().Run(context.Background())
	if err != nil {
		v.t.Fatal(err)
	}
	return findings
}

func hasRule(findings []Finding, rule, path string) *Finding {
	for i := range findings {
		if findings[i].Rule == rule && findings[i].Path == path {
			return &findings[i]
		}
	}
	return nil
}

// TestEachRuleFires provokes every rule at least once.
func TestEachRuleFires(t *testing.T) {
	t.Run(RuleNameGrammar, func(t *testing.T) {
		v := newVault(t, "")
		v.write("Financial/Alex-Rao/FY 2026/scan001.pdf", "x")
		if f := hasRule(v.run(), RuleNameGrammar, "Financial/Alex-Rao/FY 2026/scan001.pdf"); f == nil {
			t.Fatal("rule did not fire")
		} else if f.Fixable {
			t.Error("a name with no sidecar behind it must not be auto-fixable")
		}
	})

	t.Run(RuleNameNormalization, func(t *testing.T) {
		v := newVault(t, "")
		// Parses, but the grammar renders the doctype title-cased.
		v.write("Financial/Alex-Rao/FY 2026/invoice_Alex-Rao_Acme-Corp_2026.txt", "x")
		f := hasRule(v.run(), RuleNameNormalization, "Financial/Alex-Rao/FY 2026/invoice_Alex-Rao_Acme-Corp_2026.txt")
		if f == nil {
			t.Fatal("rule did not fire")
		}
		if !f.Fixable || f.Repair.MoveTo != "Financial/Alex-Rao/FY 2026/Invoice_Alex-Rao_Acme-Corp_2026.txt" {
			t.Errorf("repair: %+v", f.Repair)
		}
	})

	t.Run(RuleUnknownDocType, func(t *testing.T) {
		v := newVault(t, "")
		v.write("Financial/Alex-Rao/FY 2026/Sausage_Alex-Rao_Acme-Corp_2026.txt", "x")
		f := hasRule(v.run(), RuleUnknownDocType, "Financial/Alex-Rao/FY 2026/Sausage_Alex-Rao_Acme-Corp_2026.txt")
		if f == nil {
			t.Fatal("rule did not fire")
		}
		if f.Fixable {
			t.Error("an unknown doctype cannot be repaired without inventing a category")
		}
	})

	t.Run(RuleWrongFolder, func(t *testing.T) {
		v := newVault(t, "")
		v.write("Travel/Alex-Rao/Invoice_Alex-Rao_Acme-Corp_2026.txt", "x")
		f := hasRule(v.run(), RuleWrongFolder, "Travel/Alex-Rao/Invoice_Alex-Rao_Acme-Corp_2026.txt")
		if f == nil {
			t.Fatal("rule did not fire")
		}
		if !f.Fixable || f.Repair.MoveTo != "Financial/Alex-Rao/FY 2026/Invoice_Alex-Rao_Acme-Corp_2026.txt" {
			t.Errorf("repair: %+v", f.Repair)
		}
	})

	t.Run(RuleUnknownTag, func(t *testing.T) {
		v := newVault(t, "")
		rel := "Financial/Alex-Rao/FY 2026/Invoice_Alex-Rao_Acme-Corp_2026.txt"
		v.write(rel, "x")
		v.tag(rel, "acme-corp", "not-in-the-vocabulary")
		f := hasRule(v.run(), RuleUnknownTag, rel)
		if f == nil {
			t.Fatal("rule did not fire")
		}
		if !strings.Contains(f.Message, "not-in-the-vocabulary") {
			t.Errorf("message does not name the tag: %s", f.Message)
		}
	})

	t.Run(RuleMissingLifecycleTag, func(t *testing.T) {
		v := newVault(t, "lint:\n  require_lifecycle_tag: true\n")
		rel := "Financial/Alex-Rao/FY 2026/Invoice_Alex-Rao_Acme-Corp_2026.txt"
		v.write(rel, "x")
		v.tag(rel, "acme-corp")
		f := hasRule(v.run(), RuleMissingLifecycleTag, rel)
		if f == nil {
			t.Fatal("rule did not fire")
		}
		if f.Fixable {
			t.Error("with nothing recording a lifecycle, --fix must not guess one")
		}
	})

	t.Run(RuleMissingLifecycleTag+" with an unambiguous sidecar", func(t *testing.T) {
		v := newVault(t, "lint:\n  require_lifecycle_tag: true\n")
		rel := "Financial/Alex-Rao/FY 2026/Invoice_Alex-Rao_Acme-Corp_2026.txt"
		v.write(rel, "x")
		v.write("Financial/Alex-Rao/FY 2026/.Invoice_Alex-Rao_Acme-Corp_2026.txt.meta.yaml",
			"doctype: invoice\nsource_sha256: \"\"\nfields:\n  lifecycle: active\n")
		f := hasRule(v.run(), RuleMissingLifecycleTag, rel)
		if f == nil {
			t.Fatal("rule did not fire")
		}
		if !f.Fixable || f.Repair.AddTag != "active" {
			t.Errorf("repair: %+v", f.Repair)
		}
	})

	t.Run(RuleMultipleActive, func(t *testing.T) {
		v := newVault(t, "lint:\n  single_active_per_doctype_per_person:\n    - passport\n")
		a := "Identity/Alex-Rao/Passport_Alex-Rao_Passport-Office_2024.txt"
		b := "Identity/Alex-Rao/Passport_Alex-Rao_Passport-Office_2019.txt"
		v.write(a, "new")
		v.write(b, "old")
		v.tag(a, "active")
		v.tag(b, "active")
		findings := v.run()
		for _, rel := range []string{a, b} {
			f := hasRule(findings, RuleMultipleActive, rel)
			if f == nil {
				t.Fatalf("rule did not fire for %s", rel)
			}
			if f.Fixable {
				t.Error("which document is current is exactly the guess --fix must not make")
			}
		}
	})

	t.Run(RulePasswordInFilename, func(t *testing.T) {
		v := newVault(t, "lint:\n  forbid_passwords_in_filenames: true\n")
		rel := "Financial/Alex-Rao/FY 2026/Statement_Alex-Rao_Acme-Corp_2026_Password-Hunter2.txt"
		v.write(rel, "x")
		f := hasRule(v.run(), RulePasswordInFilename, rel)
		if f == nil {
			t.Fatal("rule did not fire")
		}
		if f.Severity != SeverityError || f.Fixable {
			t.Errorf("password findings are errors and are never auto-fixed: %+v", f)
		}
	})

	t.Run(RulePasswordInFilename+" stays quiet when disabled", func(t *testing.T) {
		v := newVault(t, "")
		rel := "Financial/Alex-Rao/FY 2026/Statement_Alex-Rao_Acme-Corp_2026_Password-Hunter2.txt"
		v.write(rel, "x")
		if f := hasRule(v.run(), RulePasswordInFilename, rel); f != nil {
			t.Error("rule fired with lint.forbid_passwords_in_filenames unset")
		}
	})

	t.Run(RuleStaleSidecar, func(t *testing.T) {
		v := newVault(t, "")
		rel := "Financial/Alex-Rao/FY 2026/Invoice_Alex-Rao_Acme-Corp_2026.txt"
		v.write(rel, "the current bytes")
		v.write("Financial/Alex-Rao/FY 2026/.Invoice_Alex-Rao_Acme-Corp_2026.txt.meta.yaml",
			"doctype: invoice\nsource_sha256: 0000000000000000000000000000000000000000000000000000000000000000\n")
		if f := hasRule(v.run(), RuleStaleSidecar, rel); f == nil {
			t.Fatal("rule did not fire")
		}
	})

	t.Run(RuleOrphanSidecar, func(t *testing.T) {
		v := newVault(t, "")
		v.write("Travel/Sam-Rao/.Ticket_Sam-Rao_Delta.txt.meta.yaml", "doctype: ticket\n")
		if f := hasRule(v.run(), RuleOrphanSidecar, "Travel/Sam-Rao/.Ticket_Sam-Rao_Delta.txt.meta.yaml"); f == nil {
			t.Fatal("rule did not fire")
		}
	})
}

func TestPasswordTokenHeuristic(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"Statement_Alex-Rao_Bank_2026_Password-Hunter2.pdf", true},
		{"Zip_Alex-Rao_Archive_2026_pw-abc123.zip", true},
		{"Card_Alex-Rao_Bank_2026_PIN.pdf", true},
		{"Invoice_Alex-Rao_Pinterest_2026.pdf", false},
		{"Invoice_Alex-Rao_Passport-Office_2024.pdf", false},
		{"Receipt_Sam-Rao_Globex_2025.txt", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, got := passwordToken(tc.name); got != tc.want {
				t.Errorf("passwordToken(%q) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

func TestTagRulesSkippedWithoutXattrSupport(t *testing.T) {
	v := newVault(t, "lint:\n  require_lifecycle_tag: true\n")
	v.write("Financial/Alex-Rao/FY 2026/Invoice_Alex-Rao_Acme-Corp_2026.txt", "x")
	l := v.linter()
	l.Search.ReadTags = func(string) ([]string, error) { return nil, tags.ErrUnsupported }
	findings, err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("lint must not fail on a filesystem without extended attributes: %v", err)
	}
	for _, f := range findings {
		if f.Rule == RuleMissingLifecycleTag || f.Rule == RuleUnknownTag {
			t.Errorf("tag rule %s fired on a filesystem that cannot store tags", f.Rule)
		}
	}
}

func TestFixMovesThroughTheMoveEngine(t *testing.T) {
	v := newVault(t, "")
	src := v.write("Travel/Alex-Rao/invoice_Alex-Rao_Acme-Corp_2026.txt", "the bytes")
	l := v.linter()
	findings, err := l.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Both the name and the folder are wrong; they must collapse into one move.
	if hasRule(findings, RuleWrongFolder, "Travel/Alex-Rao/invoice_Alex-Rao_Acme-Corp_2026.txt") == nil ||
		hasRule(findings, RuleNameNormalization, "Travel/Alex-Rao/invoice_Alex-Rao_Acme-Corp_2026.txt") == nil {
		t.Fatalf("expected both placement rules to fire: %+v", summarize(findings))
	}

	res, err := l.Fix(findings)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Fixed) != 2 {
		t.Errorf("want both placement findings reported fixed, got %+v", summarize(res.Fixed))
	}
	if res.Manifest == nil || res.Manifest.Path == "" {
		t.Fatal("a fix must write a manifest")
	}
	if len(res.Manifest.Rows) != 1 {
		t.Errorf("want one manifest row for one file, got %d", len(res.Manifest.Rows))
	}
	dst := filepath.Join(v.cfg.VaultRoot, "Financial", "Alex-Rao", "FY 2026", "Invoice_Alex-Rao_Acme-Corp_2026.txt")
	if b, err := os.ReadFile(dst); err != nil || string(b) != "the bytes" {
		t.Fatalf("document not at its conventional path: %v", err)
	}
	if _, err := os.Stat(src); err == nil {
		t.Error("the source path should have been staged")
	}
	if entries, err := os.ReadDir(v.cfg.StagingDir()); err != nil || len(entries) == 0 {
		t.Error("the source should be in the staging area, never deleted")
	}

	// The repaired vault lints clean, and rollback puts it back.
	after, err := l.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 0 {
		t.Errorf("vault should lint clean after --fix, got %+v", summarize(after))
	}
	man, err := move.ReadManifest(res.Manifest.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Engine.Rollback(man); err != nil {
		t.Fatalf("a lint fix must be reversible: %v", err)
	}
	if _, err := os.Stat(src); err != nil {
		t.Errorf("rollback did not restore the original path: %v", err)
	}
}

func TestFixSkipsWhatItCannotProve(t *testing.T) {
	v := newVault(t, "lint:\n  forbid_passwords_in_filenames: true\n  single_active_per_doctype_per_person:\n    - passport\n")
	v.write("Financial/Alex-Rao/FY 2026/scan001.pdf", "x")
	v.write("Financial/Alex-Rao/FY 2026/Sausage_Alex-Rao_Acme-Corp_2026.txt", "x")
	v.write("Financial/Alex-Rao/FY 2026/Statement_Alex-Rao_Acme-Corp_2026_Password-Hunter2.txt", "x")

	l := v.linter()
	findings, err := l.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	res, err := l.Fix(findings)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Fixed) != 0 {
		t.Errorf("nothing here is provably repairable, but --fix claimed %+v", summarize(res.Fixed))
	}
	if len(res.Skipped) != len(findings) {
		t.Errorf("every finding should have been skipped: %d of %d", len(res.Skipped), len(findings))
	}
	if res.Manifest != nil {
		t.Error("no manifest should be written when nothing moves")
	}
	if _, err := os.Stat(filepath.Join(v.cfg.VaultRoot, "Financial", "Alex-Rao", "FY 2026", "scan001.pdf")); err != nil {
		t.Errorf("--fix touched a file it could not repair: %v", err)
	}
}

func TestFixRenamesFromAnUnambiguousSidecar(t *testing.T) {
	v := newVault(t, "")
	v.write("Financial/Alex-Rao/FY 2026/scan001.txt", "invoice bytes")
	v.write("Financial/Alex-Rao/FY 2026/.scan001.txt.meta.yaml",
		"doctype: invoice\ncategory: financial\nowners:\n  - Alex Rao\nidentifier: Acme Corp\nyear: 2026\nsource_sha256: \"\"\n")

	l := v.linter()
	findings, err := l.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f := hasRule(findings, RuleNameGrammar, "Financial/Alex-Rao/FY 2026/scan001.txt")
	if f == nil || !f.Fixable {
		t.Fatalf("a sidecar carrying every required field makes the rename provable: %+v", f)
	}
	res, err := l.Fix(findings)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Fixed) != 1 {
		t.Fatalf("want one fix, got %+v", summarize(res.Fixed))
	}
	dst := filepath.Join(v.cfg.VaultRoot, "Financial", "Alex-Rao", "FY 2026", "Invoice_Alex-Rao_Acme-Corp_2026.txt")
	if _, err := os.Stat(dst); err != nil {
		t.Fatalf("renamed document missing: %v", err)
	}
	// The sidecar travels with the document.
	if _, err := os.Stat(filepath.Join(filepath.Dir(dst), ".Invoice_Alex-Rao_Acme-Corp_2026.txt.meta.yaml")); err != nil {
		t.Errorf("sidecar did not travel with the document: %v", err)
	}
}

func TestFixIsANoOpInDryRun(t *testing.T) {
	v := newVault(t, "")
	src := v.write("Travel/Alex-Rao/Invoice_Alex-Rao_Acme-Corp_2026.txt", "x")
	l := v.linter()
	l.Engine = &move.Engine{
		ManifestDir: v.cfg.ManifestDir(),
		StagingDir:  v.cfg.StagingDir(),
		DryRun:      true,
	}
	findings, err := l.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Fix(findings); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(src); err != nil {
		t.Errorf("a dry run moved a file: %v", err)
	}
}
