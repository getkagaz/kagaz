package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/getkagaz/kagaz/internal/vaultkit/audit"
	"github.com/getkagaz/kagaz/internal/vaultkit/config"
)

// VaultEnv names the environment variable that supplies a default --vault.
const VaultEnv = "KAGAZ_VAULT"

// Runtime is the state every command shares: the streams to talk on, the
// persistent flags, and the lazily-loaded vault config.
type Runtime struct {
	// Version is the build version, injected through main.version.
	Version string
	// JSON reflects the persistent --json flag.
	JSON bool
	// Vault reflects the persistent --vault flag: the path of a vault.yaml
	// file, or of a directory to start the upward search from.
	Vault string
	// Quiet suppresses human progress chatter (never JSON, never warnings).
	Quiet bool

	// Out, Err and In are the process streams, injectable for tests.
	Out io.Writer
	Err io.Writer
	In  io.Reader

	// AssumeYes is set when stdin is not a terminal and prompting is
	// impossible; commands consult Confirm rather than reading it directly.
	interactive func() bool

	cfg *config.Config
}

// NewRuntime builds a Runtime writing to the given streams.
func NewRuntime(version string, out, errw io.Writer, in io.Reader) *Runtime {
	return &Runtime{Version: version, Out: out, Err: errw, In: in}
}

// Config loads (once) and returns the vault configuration.
//
// --vault takes the path of the vault.yaml file itself; a directory is also
// accepted because humans type directories, and config.Find then walks upward
// from it. With no flag the KAGAZ_VAULT environment variable is consulted, and
// failing that the search starts at the working directory.
func (r *Runtime) Config() (*config.Config, error) {
	if r.cfg != nil {
		return r.cfg, nil
	}
	start := r.Vault
	if start == "" {
		start = os.Getenv(VaultEnv)
	}
	path, err := config.Find(start)
	if err != nil {
		if errors.Is(err, config.ErrNotFound) {
			where := start
			if where == "" {
				where = "the current directory or any parent"
			}
			return nil, fmt.Errorf("no %s found at %s; run `kagaz init` to create a vault, or pass --vault <path to vault.yaml>",
				config.FileName, where)
		}
		return nil, err
	}
	cfg, err := config.LoadFile(path)
	if err != nil {
		return nil, err
	}
	r.cfg = cfg
	return cfg, nil
}

// SetConfig installs an already-loaded config, used by `init` so that the
// vault it just wrote is the one the rest of the run sees.
func (r *Runtime) SetConfig(cfg *config.Config) { r.cfg = cfg }

// Audit opens the vault's append-only log.
func (r *Runtime) Audit(cfg *config.Config) *audit.Log { return audit.Open(cfg.AuditLogPath()) }

// Interactive reports whether a prompt can be shown: stdin must be a terminal.
func (r *Runtime) Interactive() bool {
	if r.interactive != nil {
		return r.interactive()
	}
	f, ok := r.In.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}

// Printf writes a human-facing line to stdout, unless --json or --quiet.
func (r *Runtime) Printf(format string, args ...any) {
	if r.JSON || r.Quiet {
		return
	}
	fmt.Fprintf(r.Out, format, args...)
}

// Warnf writes a warning to stderr. Warnings are never suppressed by --quiet:
// a suppressed warning is a warning that was not given.
func (r *Runtime) Warnf(format string, args ...any) {
	fmt.Fprintf(r.Err, format, args...)
}

// Prompt asks the user a question and returns the trimmed answer. It fails
// when there is no terminal, because a mutating command that cannot ask must
// refuse rather than assume consent.
func (r *Runtime) Prompt(question string) (string, error) {
	if !r.Interactive() {
		return "", errors.New("cannot prompt: stdin is not a terminal (re-run with --yes to confirm, or --propose-only to preview)")
	}
	fmt.Fprint(r.Out, question)
	reader := bufio.NewReader(r.In)
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// Confirm shows a yes/no prompt. An empty answer means no.
func (r *Runtime) Confirm(question string) (bool, error) {
	answer, err := r.Prompt(question + " [y/N] ")
	if err != nil {
		return false, err
	}
	switch strings.ToLower(answer) {
	case "y", "yes":
		return true, nil
	default:
		return false, nil
	}
}

// Emit renders a response: the JSON envelope, or the human rendering of the
// very same payload value.
func (r *Runtime) Emit(res *Response) error {
	if r.JSON {
		data, err := Envelope(res)
		if err != nil {
			return err
		}
		if _, err := r.Out.Write(data); err != nil {
			return err
		}
		return exitFor(res)
	}
	for _, w := range res.Warnings {
		r.Warnf("warning: %s\n", w)
	}
	if res.Human != nil {
		if err := res.Human(r.Out, res.Payload); err != nil {
			return err
		}
	}
	return exitFor(res)
}

// exitFor turns a non-zero response exit code into an error the root command
// hands back to main, so that emitting output and choosing an exit code stay
// one decision rather than two.
func exitFor(res *Response) error {
	if res.Exit == 0 {
		return nil
	}
	return &ExitError{Code: res.Exit, Silent: true}
}

// ExitError carries a specific process exit code out of a command.
type ExitError struct {
	// Code is the process exit code.
	Code int
	// Silent means the response has already been printed and main should not
	// print the error again.
	Silent bool
	// Err is the underlying failure, when there is one.
	Err error
}

// Error implements error.
func (e *ExitError) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	return fmt.Sprintf("exit status %d", e.Code)
}

// Unwrap exposes the underlying failure.
func (e *ExitError) Unwrap() error { return e.Err }
