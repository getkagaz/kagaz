package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/getkagaz/kagaz/internal/vaultkit/classify"
	"github.com/getkagaz/kagaz/internal/vaultkit/ocr"
)

// TestMain keeps the suite off the real kagaz-machelper.
//
// CONTRIBUTING states that every macOS-only path — Vision OCR among them — is
// tested against a recorded fixture and never by invoking the real tool. That
// held only on machines where the helper was not installed: ocr.HelperPath
// searches the Homebrew prefixes unconditionally, so `brew install kagaz` was
// enough to route extraction through real Vision OCR instead.
//
// The tests here build synthetic PDFs with a text layer and no rendered glyphs.
// pdftotext reads them exactly; Vision sees an empty page, so the rules tier
// got no text and returned unclassified, and
// TestIngestProposesForAnUnownedDocumentInAFreshVault failed with "proposal has
// no destination". CI never caught it because CI installs poppler and not the
// helper — the suite passed precisely on machines that did not have Kagaz
// installed, and failed for anyone who did.
//
// HelperPath treats an override that does not resolve as a configuration error
// rather than a reason to fall back to another binary, which is the seam this
// needs. An override already in the environment is left alone, so a test run
// can still opt in deliberately.
func TestMain(m *testing.M) {
	// Both helpers, for the same reason. TestDoctorReportsEachClassifierTiersModel
	// emptied PATH and asserted the MLX tier was unavailable -- "the MLX helper
	// is somehow available with an empty PATH" is its own failure message --
	// but ocr.FindHelper checks the Homebrew prefixes before PATH and does so
	// unconditionally, so `brew install kagaz-mlx` defeated it. PATH is not the
	// only route to a helper, and no test should assume it is.
	for _, env := range []string{ocr.HelperPathEnv, classify.MLXHelperPathEnv} {
		if _, set := os.LookupEnv(env); !set {
			os.Setenv(env, filepath.Join(os.TempDir(), "kagaz-tests-no-"+env))
		}
	}
	os.Exit(m.Run())
}
