package search

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// icloudSuffix is appended to the dot-prefixed filename of an evicted file:
// "Invoice.pdf" evicted from local storage appears as ".Invoice.pdf.icloud".
const icloudSuffix = ".icloud"

// MaterializeTimeout is the hard ceiling on waiting for iCloud to bring a file
// back. It is a failure deadline, not a hint: a caller that receives a path
// from Kagaz must be able to trust that the bytes are on this machine, so
// timing out returns an error rather than the path.
const MaterializeTimeout = 60 * time.Second

// materializePoll is how often the file is re-checked while waiting.
const materializePoll = 250 * time.Millisecond

// ErrNoBrctl means iCloud Drive's command-line client is missing. It is a real
// error rather than a graceful degradation: without brctl an evicted file
// cannot be downloaded, and reporting success would hand the caller a path
// whose bytes are not there.
var ErrNoBrctl = errors.New("brctl not found; cannot download an evicted iCloud file")

// ErrNotMaterialized means the file was still an iCloud placeholder, empty or
// missing when the deadline expired.
var ErrNotMaterialized = errors.New("file did not materialize")

// PlaceholderPath is the iCloud placeholder path for a document.
func PlaceholderPath(path string) string {
	return filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+icloudSuffix)
}

// documentForPlaceholder is the inverse of PlaceholderPath.
func documentForPlaceholder(placeholder string) string {
	base := filepath.Base(placeholder)
	base = strings.TrimSuffix(strings.TrimPrefix(base, "."), icloudSuffix)
	return filepath.Join(filepath.Dir(placeholder), base)
}

// IsEvicted reports whether path is an iCloud placeholder: its metadata is
// local but its bytes are not.
func IsEvicted(path string) bool {
	if _, err := os.Stat(PlaceholderPath(path)); err == nil {
		return true
	}
	return false
}

// Materialized reports whether path is a non-empty regular file with no iCloud
// placeholder beside it — the only state in which its bytes can be read.
func Materialized(path string) bool {
	if IsEvicted(path) {
		return false
	}
	st, err := os.Lstat(path)
	if err != nil {
		return false
	}
	return st.Mode().IsRegular() && st.Size() > 0
}

// Materialize ensures the bytes of path are on this machine, downloading it
// from iCloud with `brctl download` when it is an evicted placeholder.
//
// It fails loudly in every case it cannot guarantee the file is readable:
// brctl missing, brctl failing, or the file still not being a non-empty regular
// file when MaterializeTimeout expires. It never reports success on a
// placeholder.
func Materialize(ctx context.Context, path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if Materialized(abs) {
		return nil
	}
	if _, err := os.Lstat(abs); err != nil && !IsEvicted(abs) {
		// Neither the file nor a placeholder exists: nothing to download.
		return fmt.Errorf("materialize %s: %w", path, err)
	}

	bin, lookErr := exec.LookPath("brctl")
	if lookErr != nil {
		return fmt.Errorf("materialize %s: %w", path, ErrNoBrctl)
	}

	ctx, cancel := context.WithTimeout(ctx, MaterializeTimeout)
	defer cancel()

	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, "download", abs)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := firstLine(stderr.String()); msg != "" {
			return fmt.Errorf("materialize %s: brctl download: %w: %s", path, err, msg)
		}
		return fmt.Errorf("materialize %s: brctl download: %w", path, err)
	}

	// brctl returns as soon as the download is *requested*, so the file is not
	// necessarily readable yet. The wait below is what makes the guarantee.
	return waitMaterialized(ctx, abs, MaterializeTimeout, materializePoll)
}

// waitMaterialized blocks until path is a non-empty regular file with no iCloud
// placeholder beside it, or until timeout (or ctx) expires — in which case it
// returns ErrNotMaterialized.
//
// It is deliberately separate from Materialize and takes its own timeout and
// poll interval so the deadline behaviour can be unit-tested on any machine,
// with no brctl and no iCloud involved.
func waitMaterialized(ctx context.Context, path string, timeout, poll time.Duration) error {
	if poll <= 0 {
		poll = materializePoll
	}
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(poll)
	defer ticker.Stop()

	for {
		if Materialized(path) {
			return nil
		}
		if !time.Now().Before(deadline) {
			return notMaterialized(path, timeout)
		}
		select {
		case <-ctx.Done():
			if Materialized(path) {
				return nil
			}
			return fmt.Errorf("%s: %w: %v", path, ErrNotMaterialized, ctx.Err())
		case <-ticker.C:
		}
	}
}

// notMaterialized explains which of the failure states the file ended in.
func notMaterialized(path string, timeout time.Duration) error {
	switch {
	case IsEvicted(path):
		return fmt.Errorf("%s: %w: still an iCloud placeholder after %s", path, ErrNotMaterialized, timeout)
	default:
		st, err := os.Lstat(path)
		switch {
		case err != nil:
			return fmt.Errorf("%s: %w: file does not exist after %s", path, ErrNotMaterialized, timeout)
		case !st.Mode().IsRegular():
			return fmt.Errorf("%s: %w: not a regular file after %s", path, ErrNotMaterialized, timeout)
		default:
			return fmt.Errorf("%s: %w: file is empty after %s", path, ErrNotMaterialized, timeout)
		}
	}
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
