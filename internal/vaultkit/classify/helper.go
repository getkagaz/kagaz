package classify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/getkagaz/kagaz/internal/vaultkit/ocr"
)

// ErrNoHelper means the required helper binary is not installed. It is an
// expected condition on Linux and on a Mac without the optional helpers, not a
// bug. It mirrors ocr.ErrNoHelper's role for the classify-side binaries.
var ErrNoHelper = errors.New("kagaz helper not found")

// MLXHelperBinary is the optional Python/MLX sidecar that runs a local
// quantised LLM. It is a separate binary from kagaz-machelper because it drags
// in a Python runtime and model weights that the Swift helper does not need.
const MLXHelperBinary = "kagaz-machelper-mlx"

// MLXHelperPathEnv overrides MLX helper discovery with an explicit path, for
// development builds and tests. A packaged install never needs it.
const MLXHelperPathEnv = "KAGAZ_MACHELPER_MLX"

// mlxHelperPrefixes are the fixed directories probed last. Only the
// Apple-silicon Homebrew prefix is listed: Kagaz is Apple-silicon only, so
// /usr/local/bin is deliberately not probed.
var mlxHelperPrefixes = []string{"/opt/homebrew/bin"}

// osExecutable is a seam so tests can control the "next to the running binary"
// probe.
var osExecutable = os.Executable

// lookPath is a seam so tests can stub $PATH discovery.
var lookPath = exec.LookPath

// MLXHelperPath resolves the kagaz-machelper-mlx binary.
//
// This is intentionally parallel to ocr.HelperPath rather than a call into it:
// the search *order* is shared policy, but the binary name is not, and
// ocr.HelperPath is hard-wired to kagaz-machelper. The Apple backend in this
// package calls ocr.HelperPath directly and does not duplicate it.
//
// Search order, first hit wins:
//
//  1. $KAGAZ_MACHELPER_MLX, if it names an executable file;
//  2. the directory of the running executable;
//  3. $PATH;
//  4. the Homebrew prefix /opt/homebrew/bin.
func MLXHelperPath() (string, bool) {
	if p := os.Getenv(MLXHelperPathEnv); p != "" {
		if isExecutableFile(p) {
			return p, true
		}
		// An explicit override that does not resolve is a configuration
		// mistake, not a reason to silently run a different binary.
		return "", false
	}

	if exe, err := osExecutable(); err == nil {
		if p := filepath.Join(filepath.Dir(exe), MLXHelperBinary); isExecutableFile(p) {
			return p, true
		}
	}

	if p, err := lookPath(MLXHelperBinary); err == nil {
		return p, true
	}

	for _, dir := range mlxHelperPrefixes {
		if p := filepath.Join(dir, MLXHelperBinary); isExecutableFile(p) {
			return p, true
		}
	}
	return "", false
}

// isExecutableFile reports whether path is a regular file with an execute bit.
func isExecutableFile(path string) bool {
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		return false
	}
	return fi.Mode().Perm()&0o111 != 0
}

// helperRunner executes a helper binary with document text on stdin and returns
// its standard output. Standard output is returned even on a non-zero exit,
// because the contract puts the structured error there.
//
// It exists as a named type so tests can inject recorded helper JSON without
// executing anything, which is what lets this package's tests run on Linux CI.
type helperRunner func(ctx context.Context, path string, args []string, stdin string) ([]byte, error)

// execHelper is the production helperRunner.
//
// ocr.RunHelper is not reused here: it does not accept stdin, and the classify
// contract pipes document text in on stdin. Discovery is shared (ocr.HelperPath);
// invocation is not.
func execHelper(ctx context.Context, path string, args []string, stdin string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), helperExitError(filepath.Base(path), err, stdout.Bytes(), stderr.String())
	}
	return stdout.Bytes(), nil
}

// helperResponse is the versioned classify contract (§4.4). The same shape
// carries a structured error, so a failed helper explains itself instead of
// leaving an exit status behind.
type helperResponse struct {
	Contract   int               `json:"contract"`
	Engine     string            `json:"engine"`
	DocType    string            `json:"doctype"`
	Category   string            `json:"category"`
	Confidence float64           `json:"confidence"`
	Fields     map[string]string `json:"fields"`

	// Available and Reason are only populated by --probe.
	Available *bool  `json:"available"`
	Reason    string `json:"reason"`

	// Error is a machine-readable code, Message its human text.
	Error   string `json:"error"`
	Message string `json:"message"`
}

// helperExitError turns a non-zero helper exit into a useful error, preferring
// the structured error on stdout over the raw exit status.
func helperExitError(binary string, runErr error, stdout []byte, stderr string) error {
	var resp helperResponse
	if err := json.Unmarshal(bytes.TrimSpace(stdout), &resp); err == nil && resp.Error != "" {
		return fmt.Errorf("%s: %s", binary, describeHelperError(resp))
	}
	if s := strings.TrimSpace(stderr); s != "" {
		return fmt.Errorf("%s: %w: %s", binary, runErr, firstLine(s))
	}
	return fmt.Errorf("%s: %w", binary, runErr)
}

// describeHelperError renders a structured helper error as "message (code)".
func describeHelperError(resp helperResponse) string {
	msg := strings.TrimSpace(resp.Message)
	if msg == "" {
		return resp.Error
	}
	return fmt.Sprintf("%s (%s)", firstLine(msg), resp.Error)
}

// decodeClassifyResponse turns helper stdout into a Result. Every failure mode
// -- malformed JSON, an unknown contract version, a structured error, an empty
// doctype -- returns an error, and every one of those degrades to rules in the
// Chain rather than failing ingest.
//
// engine is the engine string to stamp on the result; the helper's own
// "engine" field is informational only, since the caller knows which binary and
// model it invoked and the helper does not get to rename itself.
func decodeClassifyResponse(binary, engine string, data []byte) (Result, error) {
	var resp helperResponse
	if err := json.Unmarshal(bytes.TrimSpace(data), &resp); err != nil {
		return Result{}, fmt.Errorf("%s classify: decoding response: %w", binary, err)
	}
	if resp.Error != "" {
		return Result{}, fmt.Errorf("%s classify: %s", binary, describeHelperError(resp))
	}
	if resp.Contract != Contract {
		return Result{}, fmt.Errorf(
			"%s classify: unsupported contract version %d (this build understands %d); upgrade kagaz or the helper so they match",
			binary, resp.Contract, Contract)
	}
	if strings.TrimSpace(resp.DocType) == "" {
		return Result{}, fmt.Errorf("%s classify: response carried no doctype", binary)
	}
	return Result{
		DocType:    resp.DocType,
		Category:   resp.Category, // recorded but overwritten by the catalog
		Confidence: resp.Confidence,
		Fields:     resp.Fields,
		Engine:     engine,
	}, nil
}

// decodeProbeResponse reads a --probe reply. A probe that cannot be understood
// counts as unavailable: an optional backend is never allowed to fail an
// operation just because its probe was strange.
func decodeProbeResponse(data []byte) (available bool, reason string) {
	var resp helperResponse
	if err := json.Unmarshal(bytes.TrimSpace(data), &resp); err != nil {
		return false, "helper probe returned unreadable JSON"
	}
	if resp.Error != "" {
		return false, describeHelperError(resp)
	}
	if resp.Contract != Contract {
		return false, fmt.Sprintf("helper speaks contract %d, this build speaks %d", resp.Contract, Contract)
	}
	if resp.Available == nil || !*resp.Available {
		if r := strings.TrimSpace(resp.Reason); r != "" {
			return false, r
		}
		return false, "helper reported the backend as unavailable"
	}
	return true, ""
}

// firstLine keeps an error to one line so CLI output stays readable.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// helperPath is a seam over ocr.HelperPath so tests can pretend
// kagaz-machelper is or is not installed without touching the ocr package.
var helperPath = ocr.HelperPath
