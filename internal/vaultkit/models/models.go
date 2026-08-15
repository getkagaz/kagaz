// Package models manages the on-disk cache of MLX model weights that backs
// `kagaz model pull`.
//
// # This is the only outbound network call in Kagaz
//
// Global Constraint 2 permits exactly one place in the entire codebase to reach
// the internet, and this is it. Everything else is on-device or localhost-only.
// That privilege comes with the obligations enforced here:
//
//   - the download host is pinned (huggingface.co), never taken from config and
//     never taken from a server response;
//   - redirects are followed only to an allowlisted host over https, because an
//     LFS download legitimately redirects to a CDN and a blanket "follow
//     redirects" is how a request ends up somewhere it was never meant to go;
//   - every downloaded file is SHA256-hashed, checked against the hub's
//     advertised LFS digest where one exists, and recorded in the manifest;
//   - the `status: ready` marker is written only after every file verifies, so
//     an interrupted pull can never be mistaken for a complete one.
//
// # Layout
//
// Weights live at `~/Library/Application Support/kagaz/models/<hf-repo>/`,
// e.g. `.../models/mlx-community/Qwen2.5-3B-Instruct-4bit/`. The Swift MLX
// helper (machelper-mlx/Sources/MacHelperMLX/ModelCache.swift) reads from
// exactly this path and is deliberately incapable of downloading anything, so
// this package is the only thing that can populate the cache. The helper
// considers a model present when the directory holds `config.json` and at
// least one `.safetensors` file; Pull refuses to mark a download ready unless
// both are true, so the two never disagree.
package models

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// CacheSubdir is the cache location relative to the user's home directory. It
// matches ModelCache.root in the Swift helper byte for byte; changing one
// without the other makes every pull invisible to the classifier.
const CacheSubdir = "Library/Application Support/kagaz/models"

// ManifestName is the download manifest, written inside a model's directory.
// The leading dot keeps it out of the way of the weight files; the Swift
// helper lists the directory and ignores anything it does not recognise.
const ManifestName = ".kagaz-model.json"

// ManifestContract is the manifest schema version. It is checked on read so a
// future format change degrades to "not ready" (re-pull) rather than being
// misread as a complete download.
const ManifestContract = 1

// Download statuses recorded in a manifest.
const (
	// StatusDownloading means a pull started and has not verified every file.
	// A directory in this state is never treated as usable.
	StatusDownloading = "downloading"
	// StatusReady means every file in the manifest was hashed and matched.
	StatusReady = "ready"
)

// Manifest records one pull, and is what makes a pull reproducible: repo,
// the concrete resolved revision (never a moving branch name), and the exact
// file list with per-file size and SHA256.
type Manifest struct {
	Contract  int    `json:"contract"`
	Repo      string `json:"repo"`
	Revision  string `json:"revision"`
	Endpoint  string `json:"endpoint"`
	Status    string `json:"status"`
	UpdatedAt string `json:"updated_at"`
	Files     []File `json:"files"`
}

// File is one downloaded file's recorded identity.
type File struct {
	// Name is the path relative to the model directory, always forward-slashed.
	Name string `json:"name"`
	// Size is the byte count actually written to disk.
	Size int64 `json:"size"`
	// SHA256 is the digest of the bytes on disk, lowercase hex.
	SHA256 string `json:"sha256"`
	// Verified is true once the on-disk bytes were hashed and matched. A
	// manifest may carry unverified entries mid-pull; Ready ignores them.
	Verified bool `json:"verified"`
}

// Store is a weights cache rooted at a directory.
type Store struct {
	// Root is the cache root. Empty means DefaultRoot.
	Root string
}

// DefaultRoot is `~/Library/Application Support/kagaz/models`.
//
// The path is spelled the same on every platform on purpose: it is macOS's
// convention and the Swift helper's hardcoded location, and keeping it
// identical elsewhere means the Linux CI exercises the real path logic rather
// than a test-only branch.
func DefaultRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("models: cannot locate home directory: %w", err)
	}
	return filepath.Join(home, filepath.FromSlash(CacheSubdir)), nil
}

// root resolves the store's root, defaulting when unset.
func (s *Store) root() (string, error) {
	if s != nil && s.Root != "" {
		return s.Root, nil
	}
	return DefaultRoot()
}

// Dir is the directory holding repo's weights.
//
// The repo id is validated rather than trusted: a component that is empty,
// "." or ".." would resolve outside the cache, and this is the one package
// that writes files from network-supplied names. The rules mirror
// ModelCache.directory(for:) in the Swift helper exactly.
func (s *Store) Dir(repo string) (string, error) {
	if err := ValidateRepo(repo); err != nil {
		return "", err
	}
	base, err := s.root()
	if err != nil {
		return "", err
	}
	return filepath.Join(append([]string{base}, strings.Split(repo, "/")...)...), nil
}

// ValidateRepo reports whether repo is a usable Hugging Face repo id
// ("org/name"), rejecting anything that is a path rather than an id.
func ValidateRepo(repo string) error {
	if strings.TrimSpace(repo) != repo || repo == "" {
		return fmt.Errorf("models: %q is not a Hugging Face repo id like org/name", repo)
	}
	if strings.HasPrefix(repo, "/") || filepath.IsAbs(repo) || strings.ContainsAny(repo, `\:`) {
		return fmt.Errorf("models: %q must be a repo id like org/name, not a path", repo)
	}
	parts := strings.Split(repo, "/")
	if len(parts) < 2 {
		return fmt.Errorf("models: %q must be a repo id like org/name", repo)
	}
	for _, p := range parts {
		if p == "" || p == "." || p == ".." {
			return fmt.Errorf("models: %q is not a valid repo id: components must not be empty, %q or %q", repo, ".", "..")
		}
		if strings.ContainsAny(p, "\x00") {
			return fmt.Errorf("models: %q contains an invalid character", repo)
		}
	}
	return nil
}

// ManifestPath is where repo's download manifest lives.
func (s *Store) ManifestPath(repo string) (string, error) {
	dir, err := s.Dir(repo)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, ManifestName), nil
}

// ReadManifest loads repo's manifest. A missing manifest returns (nil, nil):
// "never pulled" is an ordinary state, not an error.
func (s *Store) ReadManifest(repo string) (*Manifest, error) {
	path, err := s.ManifestPath(repo)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if m.Contract != ManifestContract {
		return nil, fmt.Errorf("%s: manifest contract %d is not %d; re-run `kagaz model pull %s`", path, m.Contract, ManifestContract, repo)
	}
	return &m, nil
}

// WriteManifest saves repo's manifest, stamping UpdatedAt and the contract.
// It is written before and during a download, so an interrupted pull always
// leaves a resumable record on disk.
func (s *Store) WriteManifest(repo string, m *Manifest) error {
	path, err := s.ManifestPath(repo)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	m.Contract = ManifestContract
	m.Repo = repo
	m.UpdatedAt = time.Now().UTC().Format(time.RFC3339)

	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	// Write-and-rename: a crash mid-write must not leave a manifest that
	// parses as something it is not.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Ready reports whether repo is completely downloaded and verified, and
// returns the manifest when it is. It is deliberately strict: status must be
// ready, every recorded file must be verified and still present at the
// recorded size, and the two files the Swift helper insists on (config.json
// and at least one .safetensors) must be among them. Anything less is "not
// ready", which makes a re-pull the recovery path for every partial state.
func (s *Store) Ready(repo string) (bool, *Manifest, error) {
	m, err := s.ReadManifest(repo)
	if err != nil || m == nil {
		return false, nil, err
	}
	if m.Status != StatusReady || len(m.Files) == 0 {
		return false, m, nil
	}
	dir, err := s.Dir(repo)
	if err != nil {
		return false, m, err
	}
	hasConfig, hasWeights := false, false
	for _, f := range m.Files {
		if !f.Verified || f.SHA256 == "" {
			return false, m, nil
		}
		st, err := os.Stat(filepath.Join(dir, filepath.FromSlash(f.Name)))
		if err != nil || st.Size() != f.Size {
			return false, m, nil
		}
		if f.Name == "config.json" {
			hasConfig = true
		}
		if strings.HasSuffix(f.Name, ".safetensors") {
			hasWeights = true
		}
	}
	return hasConfig && hasWeights, m, nil
}
