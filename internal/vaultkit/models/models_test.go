package models

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateRepo(t *testing.T) {
	tests := []struct {
		name string
		repo string
		ok   bool
	}{
		{"ordinary repo id", "mlx-community/Qwen2.5-3B-Instruct-4bit", true},
		{"nested namespace", "org/team/model", true},
		{"empty", "", false},
		{"no namespace", "Qwen2.5", false},
		{"absolute path", "/etc/passwd", false},
		{"leading slash with namespace", "/org/name", false},
		{"parent traversal", "org/../../etc", false},
		{"dot component", "org/./name", false},
		{"empty component", "org//name", false},
		{"backslash", `org\name`, false},
		{"surrounding whitespace", " org/name ", false},
		{"windows drive", "C:/models", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateRepo(tt.repo)
			if tt.ok && err != nil {
				t.Fatalf("ValidateRepo(%q) = %v, want nil", tt.repo, err)
			}
			if !tt.ok && err == nil {
				t.Fatalf("ValidateRepo(%q) = nil, want an error", tt.repo)
			}
		})
	}
}

// TestStoreDirMatchesSwiftHelperLayout pins the on-disk layout to what
// machelper-mlx/Sources/MacHelperMLX/ModelCache.swift reads. The helper cannot
// download anything, so if these two disagree, every pull is invisible and the
// classifier reports model_not_found on a model that is right there.
func TestStoreDirMatchesSwiftHelperLayout(t *testing.T) {
	if CacheSubdir != "Library/Application Support/kagaz/models" {
		t.Fatalf("CacheSubdir = %q; ModelCache.swift expects Library/Application Support/kagaz/models", CacheSubdir)
	}
	s := Store{Root: "/tmp/cache"}
	got, err := s.Dir("mlx-community/Qwen2.5-3B-Instruct-4bit")
	if err != nil {
		t.Fatalf("Dir: %v", err)
	}
	want := filepath.Join("/tmp/cache", "mlx-community", "Qwen2.5-3B-Instruct-4bit")
	if got != want {
		t.Fatalf("Dir = %q, want %q", got, want)
	}
	if _, err := s.Dir("../escape"); err == nil {
		t.Fatal("Dir(../escape) = nil error, want a rejection")
	}
}

// TestReadyRefusesAnythingLessThanComplete is the core safety property of the
// downloader stated from the reader's side: only a manifest that says ready,
// with every file verified and present, and with the two files the Swift
// helper insists on, counts as usable.
func TestReadyRefusesAnythingLessThanComplete(t *testing.T) {
	full := []File{
		{Name: "config.json", Size: 3, SHA256: "aa", Verified: true},
		{Name: "model.safetensors", Size: 3, SHA256: "bb", Verified: true},
	}

	tests := []struct {
		name    string
		mutate  func(*Manifest)
		onDisk  map[string]string
		wantRdy bool
	}{
		{
			name:    "complete and verified",
			onDisk:  map[string]string{"config.json": "abc", "model.safetensors": "def"},
			wantRdy: true,
		},
		{
			name:    "status still downloading",
			mutate:  func(m *Manifest) { m.Status = StatusDownloading },
			onDisk:  map[string]string{"config.json": "abc", "model.safetensors": "def"},
			wantRdy: false,
		},
		{
			name:    "one file unverified",
			mutate:  func(m *Manifest) { m.Files[1].Verified = false },
			onDisk:  map[string]string{"config.json": "abc", "model.safetensors": "def"},
			wantRdy: false,
		},
		{
			name:    "one file missing from disk",
			onDisk:  map[string]string{"config.json": "abc"},
			wantRdy: false,
		},
		{
			name:    "file truncated since the pull",
			onDisk:  map[string]string{"config.json": "abc", "model.safetensors": "d"},
			wantRdy: false,
		},
		{
			name:    "no safetensors weights",
			mutate:  func(m *Manifest) { m.Files = m.Files[:1] },
			onDisk:  map[string]string{"config.json": "abc"},
			wantRdy: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := Store{Root: t.TempDir()}
			dir, err := s.Dir("org/model")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			for name, body := range tt.onDisk {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			man := &Manifest{Revision: "deadbeef", Status: StatusReady, Files: append([]File(nil), full...)}
			if tt.mutate != nil {
				tt.mutate(man)
			}
			if err := s.WriteManifest("org/model", man); err != nil {
				t.Fatal(err)
			}
			ready, _, err := s.Ready("org/model")
			if err != nil {
				t.Fatalf("Ready: %v", err)
			}
			if ready != tt.wantRdy {
				t.Fatalf("Ready = %v, want %v", ready, tt.wantRdy)
			}
		})
	}
}

func TestReadManifestMissingIsNotAnError(t *testing.T) {
	s := Store{Root: t.TempDir()}
	m, err := s.ReadManifest("org/model")
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if m != nil {
		t.Fatalf("ReadManifest = %+v, want nil for a never-pulled model", m)
	}
}

func TestReadManifestRejectsUnknownContract(t *testing.T) {
	s := Store{Root: t.TempDir()}
	dir, _ := s.Dir("org/model")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ManifestName), []byte(`{"contract":99,"status":"ready"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadManifest("org/model"); err == nil {
		t.Fatal("ReadManifest accepted contract 99")
	}
	// And an unreadable manifest must never read as ready.
	ready, _, err := s.Ready("org/model")
	if err == nil {
		t.Fatal("Ready returned no error for an unknown contract")
	}
	if ready {
		t.Fatal("Ready = true for an unknown contract")
	}
}

// TestDefaultHostPolicy is the guard on the only outbound call in Kagaz. It
// asserts the *default* policy, not a test-injected one, because every other
// test in this file relaxes it to reach an httptest server.
func TestDefaultHostPolicy(t *testing.T) {
	tests := []struct {
		raw string
		ok  bool
	}{
		{"https://huggingface.co/api/models/org/name", true},
		{"https://cdn-lfs.huggingface.co/blob", true},
		{"https://cas-bridge.xethub.hf.co/blob", true},
		{"https://hf.co/org/name", true},
		{"http://huggingface.co/org/name", false},      // plaintext
		{"https://example.com/org/name", false},        // wrong host
		{"https://huggingface.co.evil.test/x", false},  // suffix-lookalike
		{"https://nothuggingface.co/x", false},         // suffix-lookalike
		{"https://127.0.0.1/x", false},                 // no local exfil target either
		{"ftp://huggingface.co/x", false},              // wrong scheme
		{"https://evil.test/?h=huggingface.co", false}, // query is not a host
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			u, err := url.Parse(tt.raw)
			if err != nil {
				t.Fatal(err)
			}
			err = hubHostAllowed(u)
			if tt.ok && err != nil {
				t.Fatalf("hubHostAllowed(%s) = %v, want nil", tt.raw, err)
			}
			if !tt.ok && err == nil {
				t.Fatalf("hubHostAllowed(%s) = nil, want a rejection", tt.raw)
			}
		})
	}
}

func TestLicenseNoteIsInformationalAndNamesTheModel(t *testing.T) {
	note := LicenseNote("mlx-community/Qwen2.5-3B-Instruct-4bit")
	for _, want := range []string{
		"mlx-community/Qwen2.5-3B-Instruct-4bit",
		"https://huggingface.co/mlx-community/Qwen2.5-3B-Instruct-4bit",
		"informational",
	} {
		if !strings.Contains(note, want) {
			t.Errorf("LicenseNote is missing %q:\n%s", want, note)
		}
	}
}

func TestSafeRelName(t *testing.T) {
	tests := []struct {
		name string
		ok   bool
	}{
		{"config.json", true},
		{"model-00001-of-00002.safetensors", true},
		{"sub/dir/file.json", true},
		{"../escape.json", false},
		{"a/../../escape.json", false},
		{"/absolute.json", false},
		{`back\slash.json`, false},
		{ManifestName, false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := safeRelName(tt.name)
			if tt.ok != (err == nil) {
				t.Fatalf("safeRelName(%q) = %v, want ok=%v", tt.name, err, tt.ok)
			}
		})
	}
}
