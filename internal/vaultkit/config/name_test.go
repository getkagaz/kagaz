package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNameIsOptionalAndDefaultsToTheRootFolder is the back-compatibility case:
// every vault.yaml written before `name:` existed omits it, and must keep
// loading and keep displaying something sensible.
func TestNameIsOptionalAndDefaultsToTheRootFolder(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "Family-Vault")
	path := filepath.Join(dir, "vault.yaml")
	if err := os.WriteFile(path, []byte("version: 1\nvault_root: Family-Vault\npeople:\n  - name: Alex Rao\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatalf("a vault.yaml with no name must still load: %v", err)
	}
	if cfg.Name != "" {
		t.Errorf("Name = %q; an absent name must stay empty in memory, or a round-trip "+
			"would write a name the user never authored", cfg.Name)
	}
	if got, want := cfg.DisplayName(), filepath.Base(root); got != want {
		t.Errorf("DisplayName() = %q, want the vault_root folder name %q", got, want)
	}
}

// TestExplicitNameIsUsedVerbatim: a name with punctuation and non-ASCII is a
// label, not an identifier, and must survive unmangled.
func TestExplicitNameIsUsedVerbatim(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{"ampersand and spaces", "name: Personal & Family KYC\n", "Personal & Family KYC"},
		{"quoted", "name: \"RelyWeb Corporate\"\n", "RelyWeb Corporate"},
		{"non-ascii", "name: वित्तीय — 財務\n", "वित्तीय — 財務"},
		{"colon inside quotes", "name: \"Vault: archive\"\n", "Vault: archive"},
		{"surrounding whitespace is trimmed", "name: \"  Padded  \"\n", "Padded"},
		{"at the length limit", "name: " + strings.Repeat("x", MaxNameLen) + "\n", strings.Repeat("x", MaxNameLen)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Parse([]byte("version: 1\nvault_root: ~/Documents\n" + tt.yaml))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if cfg.Name != tt.want {
				t.Errorf("Name = %q, want %q", cfg.Name, tt.want)
			}
			if cfg.DisplayName() != tt.want {
				t.Errorf("DisplayName() = %q, want %q", cfg.DisplayName(), tt.want)
			}
		})
	}
}

// TestAwkwardNamesAreRejected is the injection guard. The structural defence is
// that no path-building code reads the name at all; this is the second line,
// and it is what stops a traversal-shaped label from ever reaching a Config.
func TestAwkwardNamesAreRejected(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"parent traversal", "../../etc", ".."},
		{"bare dot dot", "..", ".."},
		{"forward slash", "Personal/KYC", "path separator"},
		{"backslash", `Personal\KYC`, "path separator"},
		{"absolute path", "/etc/passwd", "path separator"},
		{"newline breaks the markdown heading", "Personal\nKYC", "control character"},
		{"tab", "Personal\tKYC", "control character"},
		{"ansi escape could forge terminal output", "Personal\x1b[31m", "control character"},
		{"nul", "Personal\x00KYC", "control character"},
		{"too long", strings.Repeat("x", MaxNameLen+1), "maximum"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateName(tt.raw)
			if err == nil {
				t.Fatalf("ValidateName(%q) accepted a name it must reject", tt.raw)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("ValidateName(%q) = %v; want an error mentioning %q", tt.raw, err, tt.want)
			}
			// The same name must be refused through the real entry point, not
			// only by the helper.
			if _, err := Parse([]byte("version: 1\nname: " + quoteYAML(tt.raw) + "\n")); err == nil {
				t.Errorf("Parse accepted name %q", tt.raw)
			}
		})
	}
}

// TestDisplayNameFallsBackWhenTheRootHasNoBasename covers the degenerate roots
// filepath.Base cannot name.
func TestDisplayNameFallsBackWhenTheRootHasNoBasename(t *testing.T) {
	for _, root := range []string{"/", ".", ""} {
		cfg := &Config{VaultRoot: root}
		if got := cfg.DisplayName(); got != "vault" {
			t.Errorf("DisplayName() with vault_root %q = %q, want %q", root, got, "vault")
		}
	}
}

// quoteYAML renders a string as a YAML double-quoted scalar. Go's %q escapes
// are a subset of YAML's, which is why the CLI writes names the same way.
func quoteYAML(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case 0x1b:
			b.WriteString(`\e`)
		case 0:
			b.WriteString(`\0`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
