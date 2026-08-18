package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
)

// NewRootCommand builds the whole `kagaz` command tree bound to rt.
func NewRootCommand(rt *Runtime) *cobra.Command {
	var showVersion bool

	root := &cobra.Command{
		Use:   "kagaz",
		Short: "Local-first document vault manager",
		Long: "kagaz keeps a folder of documents honest: conventional filenames, Finder tags,\n" +
			"sidecar facts and an audit trail, with every mutation proposed before it happens.\n\n" +
			"Every command accepts --json for a stable machine-readable shape. Human output and\n" +
			"JSON are two renderings of the same data.",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       rt.Version,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if showVersion {
				fmt.Fprintln(rt.Out, rt.Version)
				return &ExitError{Code: ExitOK, Silent: true}
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	// The bare version string, not "kagaz version x.y.z": the Homebrew
	// formula's test block matches the version alone.
	root.SetVersionTemplate("{{.Version}}\n")
	root.SetOut(rt.Out)
	root.SetErr(rt.Err)
	root.SetIn(rt.In)

	pf := root.PersistentFlags()
	pf.StringVar(&rt.Vault, "vault", "", "path to the vault.yaml to use (a directory is also accepted)")
	pf.BoolVar(&rt.JSON, "json", false, "emit the stable JSON envelope instead of human text")
	pf.BoolVar(&rt.Quiet, "quiet", false, "suppress human progress output")
	pf.BoolVar(&showVersion, "version", false, "print the version and exit")

	root.AddCommand(
		newInitCommand(rt),
		newFindCommand(rt),
		newDoctypesCommand(rt),
		newIngestCommand(rt),
		newMoveCommand(rt),
		newTagCommand(rt),
		newSupersedeCommand(rt),
		newLintCommand(rt),
		newIndexCommand(rt),
		newRollbackCommand(rt),
		newResolveCommand(rt),
		newLogCommand(rt),
		newModelCommand(rt),
		newDoctorCommand(rt),
		newWatchCommand(rt),
		newMCPCommand(rt),
		newCompletionCommand(rt),
	)
	return root
}

// Main runs the CLI and returns the process exit code. It is the single place
// that decides how a failure is reported: as an error envelope under --json,
// as a message on stderr otherwise.
func Main(version string, args []string, out, errw io.Writer, in io.Reader) int {
	rt := NewRuntime(version, out, errw, in)
	root := NewRootCommand(rt)
	root.SetArgs(args)

	err := root.Execute()
	if err == nil {
		return ExitOK
	}

	var exit *ExitError
	if errors.As(err, &exit) {
		if exit.Silent {
			return exit.Code
		}
		reportError(rt, root, args, exit.Err, exit.Code)
		return exit.Code
	}

	code := ExitFailure
	if isUsageError(err) {
		code = ExitUsage
	}
	reportError(rt, root, args, err, code)
	return code
}

// isUsageError reports whether cobra rejected the invocation itself rather
// than the command failing at its work.
func isUsageError(err error) bool {
	msg := err.Error()
	for _, prefix := range []string{
		"unknown flag", "unknown command", "unknown shorthand", "flag needs an argument",
		"invalid argument", "accepts ", "requires at least", "requires exactly",
	} {
		if strings.HasPrefix(msg, prefix) {
			return true
		}
	}
	return false
}

// requestedJSON reports whether --json appears in the raw arguments. It is
// consulted only on the error path, where cobra may never have parsed the
// flags; a successfully parsed run uses rt.JSON.
func requestedJSON(args []string) bool {
	for _, a := range args {
		if a == "--json" || a == "--json=true" {
			return true
		}
		if a == "--" {
			return false
		}
	}
	return false
}

// reportError prints a failure once, in whichever format was asked for.
func reportError(rt *Runtime, root *cobra.Command, args []string, err error, code int) {
	if err == nil {
		return
	}
	name := "kagaz"
	if cmd, _, ferr := root.Find(args); ferr == nil && cmd != nil {
		name = cmd.Name()
	}
	// An unknown *command* fails cobra's lookup before the persistent flags are
	// parsed, so rt.JSON is still false even though --json was asked for. The
	// flag is read off the raw arguments here so that an unknown command and an
	// unknown flag produce the same envelope; a JSON caller that gets plain
	// text has no way to read the failure.
	if rt.JSON || requestedJSON(args) {
		data, encErr := Envelope(ErrorResponse(name, err, "", code))
		if encErr == nil {
			fmt.Fprint(rt.Err, string(data))
			return
		}
	}
	fmt.Fprintf(rt.Err, "kagaz: %v\n", err)
}
