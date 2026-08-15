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

// TestKeepEncryptedDocs covers all three states of the tri-state
// encrypted_docs.keep_encrypted field (Task 4). The field previously defaulted
// to false with no entry in applyDefaults, so an omitted key read as "do not
// keep documents encrypted" -- the unsafe direction. The absent case must now
// fail safe (true), and an explicit false must still be honoured, because
// "decrypt on ingest" is a legitimate choice a user can make and both
// examples/vault.yaml and the fixture vault write it explicitly. A plain bool
// defaulted to true would swallow that; this test fails if the field ever
// regresses to one.
func TestKeepEncryptedDocs(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want bool
	}{
		{
			name: "key absent defaults to true (fail safe)",
			yaml: "people:\n  - name: Alex Rao\n",
			want: true,
		},
		{
			name: "encrypted_docs present but key absent still defaults to true",
			yaml: "people:\n  - name: Alex Rao\nencrypted_docs:\n  password_store: keychain\n",
			want: true,
		},
		{
			name: "explicit true stays true",
			yaml: "people:\n  - name: Alex Rao\nencrypted_docs:\n  keep_encrypted: true\n",
			want: true,
		},
		{
			name: "explicit false is honoured, not overridden",
			yaml: "people:\n  - name: Alex Rao\nencrypted_docs:\n  keep_encrypted: false\n",
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Parse([]byte(tt.yaml))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if got := cfg.Encrypted.KeepEncryptedDocs(); got != tt.want {
				t.Fatalf("KeepEncryptedDocs() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestFixtureVaultKeepEncryptedFalseSurvives is the regression guard that the
// table test above cannot give on its own: the shipped example and fixture
// vaults both write `keep_encrypted: false`, and defaulting must not turn that
// into true behind the user's back.
func TestFixtureVaultKeepEncryptedFalseSurvives(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
	for _, rel := range []string{"examples/vault.yaml", "testdata/fixture-vault/vault.yaml"} {
		cfg, err := LoadFile(filepath.Join(repoRoot, rel))
		if err != nil {
			t.Fatalf("LoadFile(%s): %v", rel, err)
		}
		if cfg.Encrypted.KeepEncrypted == nil {
			t.Fatalf("%s: expected an explicit keep_encrypted value", rel)
		}
		if cfg.Encrypted.KeepEncryptedDocs() {
			t.Errorf("%s: explicit keep_encrypted: false was overridden to true", rel)
		}
	}
}

// TestRequireLocalhost pins the endpoint allow-list, including the one entry
// that was removed (Task 3): 0.0.0.0 is a bind address, not a destination.
// Accepting it here while both Ollama call sites reject it at request time
// meant a vault.yaml could validate cleanly and then fail at the first
// classification -- the user learned about the mistake at the worst moment.
func TestRequireLocalhost(t *testing.T) {
	tests := []struct {
		endpoint string
		wantErr  bool
	}{
		{"", false},
		{"http://localhost:11434", false},
		{"http://127.0.0.1:11434", false},
		{"http://[::1]:11434", false},
		{"http://LOCALHOST:11434/api", false},
		{"http://0.0.0.0:11434", true},
		{"http://192.168.1.10:11434", true},
		{"https://api.example.com", true},
		{"http://user@evil.example.com:11434", true},
	}
	for _, tt := range tests {
		t.Run(tt.endpoint, func(t *testing.T) {
			err := requireLocalhost(tt.endpoint)
			if (err != nil) != tt.wantErr {
				t.Fatalf("requireLocalhost(%q) = %v, wantErr %v", tt.endpoint, err, tt.wantErr)
			}
		})
	}
}

// TestZeroAddressEndpointIsRejectedByParse checks the same rule where a user
// actually meets it: loading a vault.yaml.
func TestZeroAddressEndpointIsRejectedByParse(t *testing.T) {
	for _, yaml := range []string{
		"people:\n  - name: Alex Rao\nclassify:\n  endpoint: \"http://0.0.0.0:11434\"\n",
		"people:\n  - name: Alex Rao\nocr:\n  ollama:\n    endpoint: \"http://0.0.0.0:11434\"\n",
	} {
		if _, err := Parse([]byte(yaml)); err == nil {
			t.Errorf("0.0.0.0 endpoint was accepted:\n%s", yaml)
		}
	}
}
