package config

import (
	"path/filepath"
	"runtime"
	"testing"
)

// TestExampleAndFixtureVaultsLoad re-confirms (Task 10 fix round 1) that both
// examples/vault.yaml and testdata/fixture-vault/vault.yaml still parse
// under KnownFields(true) after edits to either the docs or the fixture --
// a field typo or an owner-folder path fix in the fixture is exactly the
// kind of change that silently breaks this without a directly loading test.
func TestExampleAndFixtureVaultsLoad(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..", "..")

	for _, rel := range []string{
		"examples/vault.yaml",
		"testdata/fixture-vault/vault.yaml",
	} {
		path := filepath.Join(repoRoot, rel)
		cfg, err := LoadFile(path)
		if err != nil {
			t.Fatalf("LoadFile(%s): %v", rel, err)
		}
		if len(cfg.People) == 0 {
			t.Errorf("%s: expected at least one person", rel)
		}
		if len(cfg.Structure) == 0 {
			t.Errorf("%s: expected at least one structure category", rel)
		}
		if !cfg.Confidence.ConfirmationRequired() {
			t.Errorf("%s: ConfirmationRequired() was false", rel)
		}
	}
}

// TestConfirmationRequired covers all three states of the tri-state
// require_confirmation_on_resolve_for_send field (Task 10 fix round 2):
// key absent, explicit true, and explicit false. The absent and false cases
// are the ones that matter most -- absent must fail closed (true), and an
// explicit false must be honoured, not silently overridden. A plain bool
// cannot represent this distinction, which is exactly the bug round 2 fixed;
// this test is written so it genuinely fails if the field ever regresses to
// a plain bool.
func TestConfirmationRequired(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want bool
	}{
		{
			name: "key absent defaults to true (fail closed)",
			yaml: "people:\n  - name: Alex Rao\n",
			want: true,
		},
		{
			name: "explicit true stays true",
			yaml: "people:\n  - name: Alex Rao\nconfidential:\n  require_confirmation_on_resolve_for_send: true\n",
			want: true,
		},
		{
			name: "explicit false is honoured, not overridden",
			yaml: "people:\n  - name: Alex Rao\nconfidential:\n  require_confirmation_on_resolve_for_send: false\n",
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Parse([]byte(tt.yaml))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if got := cfg.Confidence.ConfirmationRequired(); got != tt.want {
				t.Fatalf("ConfirmationRequired() = %v, want %v", got, tt.want)
			}
		})
	}
}
