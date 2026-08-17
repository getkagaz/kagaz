package index

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/getkagaz/kagaz/internal/vaultkit/config"
	"github.com/getkagaz/kagaz/internal/vaultkit/search"
	"github.com/getkagaz/kagaz/internal/vaultkit/tags"
)

// update rewrites the golden files. Run `go test ./internal/vaultkit/index
// -update` after an intentional change to the generated output, and read the
// resulting diff — that diff is the review.
var update = flag.Bool("update", false, "rewrite the golden files")

const (
	fixtureVault = "../../../testdata/fixture-vault/vault.yaml"
	docsTemplate = "../../../docs/AGENTS.template.md"
)

func fixtureConfig(t *testing.T) *config.Config {
	t.Helper()
	abs, err := filepath.Abs(fixtureVault)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadFile(abs)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func fixtureRender(t *testing.T) (indexMD, agentsMD string) {
	t.Helper()
	cfg := fixtureConfig(t)
	s, err := search.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Tag reading is pinned to "supported, none set" rather than left to the
	// host filesystem. Linux rejects the Apple tag xattr namespace outright
	// (EOPNOTSUPP) while APFS reports simply "no attribute", and INDEX.md now
	// says different things about those two states — correctly. The golden file
	// pins what the vault contains, not what the filesystem under it can do.
	s.ReadTags = func(string) ([]string, error) { return nil, nil }
	tree, err := s.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	g, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	idx, err := g.Index(tree)
	if err != nil {
		t.Fatal(err)
	}
	agents, err := g.Agents(tree)
	if err != nil {
		t.Fatal(err)
	}
	return idx, agents
}

func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run `go test ./internal/vaultkit/index -update` to create it)", err)
	}
	if got != string(want) {
		t.Errorf("%s differs from the golden file.\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

func TestGoldenIndex(t *testing.T) {
	idx, _ := fixtureRender(t)
	checkGolden(t, "INDEX.golden.md", idx)
}

func TestGoldenAgents(t *testing.T) {
	_, agents := fixtureRender(t)
	checkGolden(t, "AGENTS.golden.md", agents)
}

// TestTemplateMatchesDocs is what keeps the embedded copy honest. go:embed
// cannot reach docs/AGENTS.template.md from this package, so the template is
// copied in; this test fails the moment the two diverge.
func TestTemplateMatchesDocs(t *testing.T) {
	want, err := os.ReadFile(docsTemplate)
	if err != nil {
		t.Fatal(err)
	}
	if Template() != string(want) {
		t.Error("internal/vaultkit/index/template/AGENTS.template.md has drifted from docs/AGENTS.template.md; copy the docs file over it")
	}
}

// TestOutputIsByteStable renders repeatedly in one process. Go randomises map
// iteration per range, so a rendering that leaked a map's order would differ
// across these iterations rather than only across machines.
func TestOutputIsByteStable(t *testing.T) {
	idx, agents := fixtureRender(t)
	for i := 0; i < 25; i++ {
		gotIdx, gotAgents := fixtureRender(t)
		if gotIdx != idx {
			t.Fatalf("INDEX.md is not byte-stable (iteration %d)", i)
		}
		if gotAgents != agents {
			t.Fatalf("AGENTS.md is not byte-stable (iteration %d)", i)
		}
	}
}

// TestNoMachineSpecificContent guards the properties that make the golden files
// usable at all: no timestamp, no duration, no absolute path.
func TestNoMachineSpecificContent(t *testing.T) {
	cfg := fixtureConfig(t)
	idx, agents := fixtureRender(t)
	dateRe := regexp.MustCompile(`\b20\d\d-\d\d-\d\d\b`)
	clockRe := regexp.MustCompile(`\b\d\d:\d\d(:\d\d)?\b`)
	for name, body := range map[string]string{"INDEX.md": idx, "AGENTS.md": agents} {
		if strings.Contains(body, cfg.VaultRoot) {
			t.Errorf("%s contains the absolute vault root", name)
		}
		if strings.Contains(body, "/Users/") || strings.Contains(body, "/home/") {
			t.Errorf("%s contains an absolute home path", name)
		}
		if m := dateRe.FindString(body); m != "" {
			t.Errorf("%s contains a date (%s); generated output must not carry a timestamp", name, m)
		}
		if m := clockRe.FindString(body); m != "" {
			t.Errorf("%s contains a clock time (%s)", name, m)
		}
		if !strings.Contains(body, Banner) {
			t.Errorf("%s is missing the GENERATED banner", name)
		}
	}
}

func TestAgentsSubstitutesEveryPlaceholder(t *testing.T) {
	_, agents := fixtureRender(t)
	if left := leftoverRe.FindAllString(agents, -1); len(left) > 0 {
		t.Fatalf("unsubstituted placeholders: %q", left)
	}
	for _, want := range []string{
		"# AGENTS.md — fixture-vault",
		"{DocType}_{Names}_{Identifier}[_{Year}][_{Modifier}]",
		"**Alex Rao** — tag `alex-rao`",
		"`invoice` — `financial`",
		"kMDItemUserTags",
	} {
		if !strings.Contains(agents, want) {
			t.Errorf("AGENTS.md is missing %q", want)
		}
	}
	// The Jekyll front matter and the placeholder-contract comment are for the
	// docs site and for this package's implementer; neither belongs in a user's
	// vault.
	if strings.HasPrefix(agents, "---") {
		t.Error("AGENTS.md still carries the template's front matter")
	}
	if strings.Contains(agents, "Placeholder contract") {
		t.Error("AGENTS.md still carries the template's implementation comment")
	}
}

// TestUnknownPlaceholderIsAnError: a template that grows a token this build does
// not know must fail loudly rather than ship `{KAGAZ_...}` into a vault.
func TestUnknownPlaceholderIsAnError(t *testing.T) {
	original := templateSource
	t.Cleanup(func() { templateSource = original })
	templateSource = "# {KAGAZ_VAULT_NAME}\n\n{KAGAZ_SOMETHING_NEW}\n"

	cfg := fixtureConfig(t)
	g, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, err = g.Agents(&search.Tree{})
	if err == nil {
		t.Fatal("want an error naming the unknown placeholder")
	}
	if !strings.Contains(err.Error(), "{KAGAZ_SOMETHING_NEW}") {
		t.Errorf("error should name the placeholder: %v", err)
	}
}

func TestEmptyVaultRendersNoneYetRatherThanBlanks(t *testing.T) {
	dir := t.TempDir()
	yaml := "version: 1\nvault_root: .\nstructure:\n  financial:\n    path: Financial\n    layout: \"{Owner}\"\n" +
		"tags:\n  lifecycle: []\n"
	path := filepath.Join(dir, "vault.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	g, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	agents, err := g.Agents(&search.Tree{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(agents, noneYet) {
		t.Error("an empty list must render as an explicit (none yet), never as a blank or a removed section")
	}
	idx, err := g.Index(&search.Tree{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(idx, "| `financial` |") {
		t.Error("a category with no documents must still be listed")
	}
}

// TestVaultNameComesFromConfigWithAFolderFallback: both generated documents
// title themselves with the vault's label. The fixture vault sets no `name:`,
// which is the back-compatible case the goldens pin; an explicit name replaces
// the folder name in both files and nowhere else.
func TestVaultNameComesFromConfigWithAFolderFallback(t *testing.T) {
	tests := []struct {
		name    string
		nameKey string
		want    string
	}{
		{"absent name falls back to the root folder", "", "named-vault"},
		{"explicit name is used", "name: Personal & Family KYC\n", "Personal & Family KYC"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "named-vault")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			yaml := "version: 1\n" + tt.nameKey + "vault_root: .\n" +
				"structure:\n  financial:\n    path: Financial\n    layout: \"{Owner}\"\n"
			path := filepath.Join(dir, "vault.yaml")
			if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.LoadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			g, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if got := g.VaultName(); got != tt.want {
				t.Fatalf("VaultName() = %q, want %q", got, tt.want)
			}
			idx, err := g.Index(&search.Tree{})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(idx, "# INDEX — "+tt.want+"\n") {
				t.Errorf("INDEX.md title does not carry the vault name:\n%s", firstLine(idx))
			}
			agents, err := g.Agents(&search.Tree{})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(agents, "# AGENTS.md — "+tt.want+"\n") {
				t.Errorf("AGENTS.md heading does not carry the vault name:\n%s", firstLine(agents))
			}
		})
	}
}

func firstLine(s string) string {
	if i := strings.Index(s, "\n"); i >= 0 {
		return s[:i]
	}
	return s
}

func TestWriteProducesBothFiles(t *testing.T) {
	dir := t.TempDir()
	yaml := "version: 1\nvault_root: .\nstructure:\n  financial:\n    path: Financial\n    layout: \"{Owner}\"\n"
	if err := os.WriteFile(filepath.Join(dir, "vault.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadFile(filepath.Join(dir, "vault.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	paths, err := Generate(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 {
		t.Fatalf("want two files written, got %v", paths)
	}
	first := make([]string, 2)
	for i, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		first[i] = string(b)
	}
	// Regenerating an unchanged vault must not change a byte, so a vault kept
	// in git shows no diff.
	if _, err := Generate(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	for i, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != first[i] {
			t.Errorf("%s changed on a second run over an unchanged vault", filepath.Base(p))
		}
	}
}

// TestTagsUnsupportedIsReportedAsSuch is F6: "no tags are set" and "tags cannot
// be read here" are different facts, and a vault synced onto a filesystem
// without extended attributes has not lost its tags.
func TestTagsUnsupportedIsReportedAsSuch(t *testing.T) {
	cfg := fixtureConfig(t)
	s, err := search.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.ReadTags = func(string) ([]string, error) { return nil, tags.ErrUnsupported }
	tree, err := s.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	g, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	idx, err := g.Index(tree)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(idx, "No Finder tags are set") {
		t.Error("INDEX.md claims nothing is tagged on a filesystem that cannot read tags")
	}
	if !strings.Contains(idx, "cannot be read on this filesystem") {
		t.Error("INDEX.md does not explain that tags are unreadable here")
	}
}
