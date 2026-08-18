package classify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/getkagaz/kagaz/internal/vaultkit/ocr"
)

// MLXHelperBinary is the optional Python/MLX sidecar that runs a local
// quantised LLM. It is a separate binary from kagaz-machelper because it drags
// in a Python runtime and model weights that the Swift helper does not need.
const MLXHelperBinary = "kagaz-machelper-mlx"

// MLXHelperPathEnv overrides MLX helper discovery with an explicit path, for
// development builds and tests. A packaged install never needs it.
const MLXHelperPathEnv = "KAGAZ_MACHELPER_MLX"

// maxHelperOutput bounds how much of a helper's stdout is retained. The
// contract payload is a small JSON object; a helper that streams megabytes at
// us is malfunctioning, and buffering it unboundedly would be the malfunction
// becoming ours.
const maxHelperOutput = 1 << 20 // 1 MiB

// maxHelperStderr bounds retained stderr. Only the first line is ever reported.
const maxHelperStderr = 64 << 10

// helperWaitDelay bounds the gap between killing a helper and giving up on its
// pipes.
//
// exec.CommandContext kills only the direct child. A helper that forks workers
// (the MLX one loads weights in subprocesses) leaves those workers holding the
// inherited stdout/stderr pipes, and cmd.Wait blocks until every writer closes
// them -- so without WaitDelay a timed-out classification hangs forever, past
// its own deadline, with orphaned workers pinning gigabytes of weights.
const helperWaitDelay = 3 * time.Second

// Helper failure codes minted by the Go side, alongside the codes the helper
// itself reports (`backend_unavailable`, `no_text`, ...). They exist so
// `kagaz doctor` can tell "the model refused" from "the helper hung" from "the
// helper is speaking a contract we do not know".
const (
	// CodeTimeout means the helper did not answer within its budget.
	CodeTimeout = "timeout"
	// CodeBadResponse means the helper's output was not decodable.
	CodeBadResponse = "bad_response"
	// CodeUnsupportedContract means the helper spoke a contract version this
	// build does not understand.
	CodeUnsupportedContract = "unsupported_contract"
)

// MLXHelperPath resolves the kagaz-machelper-mlx binary through the
// repository's single helper-discovery implementation, ocr.FindHelper. The
// binary name and its override variable live here because the MLX helper is
// classify's dependency, not OCR's; the search order does not.
func MLXHelperPath() (string, bool) {
	return ocr.FindHelper(MLXHelperBinary, MLXHelperPathEnv)
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
// contract pipes document text in on stdin. Discovery (ocr.FindHelper), the
// error type (ocr.HelperFailure) and the sentinel (ocr.ErrNoHelper) are all
// shared with the ocr package; only the invocation differs.
func execHelper(ctx context.Context, path string, args []string, stdin string) ([]byte, error) {
	stdout := &boundedBuffer{limit: maxHelperOutput}
	stderr := &boundedBuffer{limit: maxHelperStderr}

	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	// See helperWaitDelay: without this, an orphaned grandchild holding the
	// output pipe makes Wait block past the deadline, forever.
	cmd.WaitDelay = helperWaitDelay

	err := cmd.Run()
	if err == nil {
		return stdout.Bytes(), nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return stdout.Bytes(), &ocr.HelperFailure{
			Binary:  filepath.Base(path),
			Code:    CodeTimeout,
			Message: "helper did not answer within its time budget",
			Stdout:  stdout.Bytes(),
			Err:     ctxErr,
		}
	}
	return stdout.Bytes(), helperFailure(filepath.Base(path), err, stdout.Bytes(), stderr.String())
}

// boundedBuffer is an io.Writer that keeps at most limit bytes and silently
// drops the rest. It reports every write as fully accepted so the child is
// never handed a short-write error, which would turn "chatty helper" into
// "helper died with EPIPE".
type boundedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.buf.Len(); room > 0 {
		if len(p) <= room {
			b.buf.Write(p)
		} else {
			b.buf.Write(p[:room])
		}
	}
	return len(p), nil
}

// Bytes returns the retained output.
func (b *boundedBuffer) Bytes() []byte { return b.buf.Bytes() }

// String returns the retained output as a string.
func (b *boundedBuffer) String() string { return b.buf.String() }

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

	// Available, Reason and ReasonCode are only populated by --probe.
	// Reason is prose for a human; ReasonCode is the stable vocabulary the
	// Settings window keys its actions off, so no client ever has to
	// pattern-match an English sentence to know WHICH precondition failed.
	Available  *bool  `json:"available"`
	Reason     string `json:"reason"`
	ReasonCode string `json:"reason_code"`

	// Error is a machine-readable code, Message its human text.
	Error   string `json:"error"`
	Message string `json:"message"`
}

// helperFailure turns a non-zero helper exit into an *ocr.HelperFailure,
// preferring the structured error on stdout over the raw exit status, so
// callers can switch on Code rather than parse a sentence.
func helperFailure(binary string, runErr error, stdout []byte, stderr string) error {
	var resp helperResponse
	if err := json.Unmarshal(bytes.TrimSpace(stdout), &resp); err == nil && resp.Error != "" {
		return &ocr.HelperFailure{
			Binary:  binary,
			Code:    resp.Error,
			Message: resp.Message,
			Stdout:  stdout,
			Err:     runErr,
		}
	}
	return &ocr.HelperFailure{
		Binary:  binary,
		Message: firstLine(strings.TrimSpace(stderr)),
		Stdout:  stdout,
		Err:     runErr,
	}
}

// decodeClassifyResponse turns helper stdout into a Result. Every failure mode
// -- malformed JSON, an unknown contract version, a structured error, an empty
// doctype -- returns an *ocr.HelperFailure carrying a Code, and every one of
// those degrades to rules in the Chain rather than failing ingest.
//
// binary names the helper for error messages -- the MLX tier and the default
// tier are different binaries, and an error naming the wrong one sends the
// user to fix a helper that is working. engine is the engine string to stamp
// on the result; the helper's own
// "engine" field is informational only, since the caller knows which binary and
// model it invoked and the helper does not get to rename itself.
func decodeClassifyResponse(binary, engine string, data []byte) (Result, error) {
	var resp helperResponse
	if err := json.Unmarshal(bytes.TrimSpace(data), &resp); err != nil {
		return Result{}, &ocr.HelperFailure{
			Binary:  binary,
			Code:    CodeBadResponse,
			Message: "decoding response: " + err.Error(),
			Stdout:  data,
			Err:     err,
		}
	}
	if resp.Error != "" {
		return Result{}, &ocr.HelperFailure{Binary: binary, Code: resp.Error, Message: resp.Message, Stdout: data}
	}
	if resp.Contract != Contract {
		return Result{}, &ocr.HelperFailure{
			Binary: binary,
			Code:   CodeUnsupportedContract,
			Message: "helper speaks contract " + strconv.Itoa(resp.Contract) + ", this build speaks " + strconv.Itoa(Contract) +
				"; upgrade kagaz or the helper so they match",
			Stdout: data,
		}
	}
	if strings.TrimSpace(resp.DocType) == "" {
		return Result{}, &ocr.HelperFailure{
			Binary:  binary,
			Code:    CodeBadResponse,
			Message: "response carried no doctype",
			Stdout:  data,
		}
	}
	return Result{
		DocType:    resp.DocType,
		Category:   resp.Category, // recorded but overwritten by the catalog
		Confidence: resp.Confidence,
		Fields:     resp.Fields,
		Engine:     engine,
	}, nil
}

// Reason codes: WHICH precondition a classifier tier is missing, in a stable
// machine-readable form.
//
// `kagaz doctor` reports one alongside the prose detail on every unavailable
// classifier check. The prose is for a human and is reworded freely; these
// values are an API and are not, because a client that has to grep an English
// sentence to decide whether to offer a 1.6 GB download breaks the first time
// somebody improves the wording.
//
// The distinction that matters most is ReasonWeightsMissing versus
// ReasonHelperMissing / ReasonShaderLibraryMissing: only the first is fixed by
// `kagaz model pull`. Offering a download for either of the others would make a
// user wait minutes for nothing to change.
const (
	// ReasonHelperMissing: the helper binary is not installed or not found.
	ReasonHelperMissing = "helper_missing"
	// ReasonWeightsMissing: the helper is installed and working, but the model
	// weights are absent or the download is incomplete. THE one code that
	// `kagaz model pull` fixes.
	ReasonWeightsMissing = "weights_missing"
	// ReasonShaderLibraryMissing: the MLX helper is installed but its compiled
	// Metal shader library is not beside it. Rebuilt, never downloaded.
	ReasonShaderLibraryMissing = "shader_library_missing"
	// ReasonNoMetalDevice: no Apple silicon GPU for MLX to run on.
	ReasonNoMetalDevice = "no_metal_device"
	// ReasonModelNotConfigured: classify.model is empty.
	ReasonModelNotConfigured = "model_not_configured"
	// ReasonOSUnsupported: this macOS cannot host the tier at all -- Apple
	// Foundation Models need macOS 26, and no download changes that.
	ReasonOSUnsupported = "os_unsupported"
	// ReasonModelUnavailable: the OS supports the tier but the model is not
	// usable right now (still downloading, device ineligible, turned off).
	ReasonModelUnavailable = "model_unavailable"
	// ReasonDaemonUnreachable: no Ollama server answered at the endpoint.
	ReasonDaemonUnreachable = "daemon_unreachable"
	// ReasonModelNotPulled: the Ollama daemon answers but has not pulled the
	// configured model.
	ReasonModelNotPulled = "model_not_pulled"
	// ReasonProbeTimeout: the probe did not answer in time. Not cached, and
	// usually a cold start rather than a missing anything.
	ReasonProbeTimeout = "probe_timeout"
	// ReasonContractMismatch: the helper speaks a different contract version.
	ReasonContractMismatch = "contract_mismatch"
	// ReasonUnreadableProbe: the probe reply could not be parsed.
	ReasonUnreadableProbe = "unreadable_probe"
	// ReasonUnknown: unavailable for a reason the helper did not classify.
	ReasonUnknown = "unknown"
)

// decodeProbeResponse reads a --probe reply. A probe that cannot be understood
// counts as unavailable: an optional backend is never allowed to fail an
// operation just because its probe was strange.
func decodeProbeResponse(data []byte) (available bool, reason, code string) {
	var resp helperResponse
	if err := json.Unmarshal(bytes.TrimSpace(data), &resp); err != nil {
		return false, "helper probe returned unreadable JSON", ReasonUnreadableProbe
	}
	if resp.Error != "" {
		return false, describeHelperError(resp.Error, resp.Message), reasonCodeOr(resp.ReasonCode, ReasonUnknown)
	}
	if resp.Contract != Contract {
		return false,
			"helper speaks contract " + strconv.Itoa(resp.Contract) + ", this build speaks " + strconv.Itoa(Contract),
			ReasonContractMismatch
	}
	if resp.Available == nil || !*resp.Available {
		if r := strings.TrimSpace(resp.Reason); r != "" {
			return false, r, reasonCodeOr(resp.ReasonCode, ReasonUnknown)
		}
		return false, "helper reported the backend as unavailable", reasonCodeOr(resp.ReasonCode, ReasonUnknown)
	}
	return true, "", ""
}

// reasonCodeOr returns the helper's own code, or a fallback when it did not
// send one -- an older helper, built before the field existed, simply reports
// "unknown" rather than making the caller guess from the prose.
func reasonCodeOr(code, fallback string) string {
	if c := strings.TrimSpace(code); c != "" {
		return c
	}
	return fallback
}

// describeHelperError renders a structured helper error as "message (code)".
func describeHelperError(code, message string) string {
	message = strings.TrimSpace(message)
	if message == "" {
		return code
	}
	return firstLine(message) + " (" + code + ")"
}

// isTimeout reports whether an error is a deadline or cancellation, including
// the ocr.HelperFailure wrapper execHelper builds around one.
func isTimeout(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
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
