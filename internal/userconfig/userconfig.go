// Package userconfig reads and writes the per-user settings file that the CLI
// and the Kagaz app both consult.
//
// It holds one thing today: which vault to use when nothing else says. That
// exists because the app and the CLI otherwise have no way to agree. The app
// remembers its vault in macOS user defaults, which a terminal cannot read, so
// a user who picks a vault in the app and then runs `kagaz find` from their
// home directory is told no vault exists — and the advice that follows, `kagaz
// init`, creates a second one. A shared file is the smallest thing that stops
// the two halves disagreeing, and it is the shape the comparable tools use:
// Claude Code shares ~/.claude, Codex shares ~/.codex/config.toml, in both
// cases read by the GUI and the command line alike.
//
// It is deliberately NOT a mirror of the app's preferences. Appearance,
// density and the dashboard's tag are the app's business and stay in user
// defaults; only what both halves must agree on belongs here.
package userconfig

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// FileName is the settings file's name inside the kagaz config directory.
const FileName = "config.yaml"

// EnvDir overrides the config directory. It exists for tests and for a user
// who keeps dotfiles somewhere unusual; it is read before XDG_CONFIG_HOME.
const EnvDir = "KAGAZ_CONFIG_DIR"

// Settings is the file's contents.
type Settings struct {
	// Vault is the vault.yaml to use when no --vault flag, no KAGAZ_VAULT and
	// no vault above the working directory say otherwise. Absent means the
	// user has never chosen one, which is different from choosing none.
	Vault string `yaml:"vault,omitempty"`
}

// Dir returns the directory holding the settings file.
//
// $KAGAZ_CONFIG_DIR wins, then $XDG_CONFIG_HOME/kagaz, then ~/.config/kagaz.
// The XDG default is spelled out rather than taken from os.UserConfigDir,
// which answers ~/Library/Application Support on macOS — a path no one types
// and no dotfile manager tracks.
func Dir() (string, error) {
	if dir := os.Getenv(EnvDir); dir != "" {
		return dir, nil
	}
	if base := os.Getenv("XDG_CONFIG_HOME"); base != "" {
		return filepath.Join(base, "kagaz"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot locate a home directory: %w", err)
	}
	return filepath.Join(home, ".config", "kagaz"), nil
}

// Path returns the settings file's full path.
func Path() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, FileName), nil
}

// Load reads the settings.
//
// A missing file is not an error: it means nothing has been configured, which
// is the normal state of a fresh machine and must not stop a command that was
// going to find its vault some other way.
func Load() (Settings, error) {
	path, err := Path()
	if err != nil {
		return Settings{}, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Settings{}, nil
		}
		return Settings{}, fmt.Errorf("reading %s: %w", path, err)
	}
	var s Settings
	if err := yaml.Unmarshal(raw, &s); err != nil {
		return Settings{}, fmt.Errorf("%s is not valid YAML: %w", path, err)
	}
	return s, nil
}

// Save writes the settings, creating the directory if needed.
//
// The write goes to a temporary file in the same directory and is renamed over
// the target, so a reader never sees a half-written file: the app writes this
// on every vault switch and a CLI in another terminal may be reading it at
// that moment.
func Save(s Settings) error {
	dir, err := Dir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	raw, err := yaml.Marshal(s)
	if err != nil {
		return err
	}
	header := []byte("# Written by kagaz and by the Kagaz app. Safe to edit or delete.\n" +
		"# `kagaz use <path to vault.yaml>` sets the vault below.\n")

	tmp, err := os.CreateTemp(dir, FileName+".*")
	if err != nil {
		return fmt.Errorf("creating a temporary file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // a no-op once the rename has succeeded
	if _, err := tmp.Write(append(header, raw...)); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return err
	}
	path := filepath.Join(dir, FileName)
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}
