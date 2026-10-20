package cli

import (
	"fmt"
	"io"
	"path/filepath"

	"github.com/getkagaz/kagaz/internal/userconfig"
	"github.com/getkagaz/kagaz/internal/vaultkit/config"
	"github.com/spf13/cobra"
)

// UsePayload is what `kagaz use` reports.
type UsePayload struct {
	// Vault is the saved vault.yaml path, absent when none is saved.
	Vault string `json:"vault,omitempty"`
	// Settings is the file the answer came from or went to, always reported
	// so a user who wants to edit or delete it knows where it is.
	Settings string `json:"settings"`
	// Changed is true when this run wrote something, false when it only read.
	Changed bool `json:"changed"`
}

func newUseCommand(rt *Runtime) *cobra.Command {
	var clear bool

	cmd := &cobra.Command{
		Use:   "use [path to vault.yaml]",
		Short: "Set the vault to use when no --vault is given",
		Long: "use records which vault kagaz should fall back to, in a settings file this\n" +
			"machine's Kagaz app reads and writes too. It is how the app and the terminal\n" +
			"agree on one vault instead of each having its own idea.\n\n" +
			"The saved vault is consulted last: --vault wins, then KAGAZ_VAULT, then any\n" +
			"vault above the working directory. Standing inside a vault still uses that one.\n\n" +
			"With no argument it prints what is saved. Nothing is moved, written or read\n" +
			"inside the vault itself.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			settingsPath, err := userconfig.Path()
			if err != nil {
				return err
			}

			if clear {
				if err := userconfig.Save(userconfig.Settings{}); err != nil {
					return err
				}
				return rt.Emit(&Response{
					Command: "use",
					Status:  StatusOK,
					Payload: UsePayload{Settings: settingsPath, Changed: true},
					Human:   humanUse,
				})
			}

			// No argument: report, do not guess.
			if len(args) == 0 {
				saved, err := userconfig.Load()
				if err != nil {
					return err
				}
				return rt.Emit(&Response{
					Command: "use",
					Status:  StatusOK,
					Payload: UsePayload{Vault: saved.Vault, Settings: settingsPath},
					Human:   humanUse,
				})
			}

			// Resolve before saving. A path that is not a vault saved now is a
			// failure deferred to every later command, each of which would
			// report it as "no vault found" far from the decision that caused
			// it. config.Find accepts a directory, so this also turns what the
			// user typed into the vault.yaml itself.
			found, err := config.Find(args[0])
			if err != nil {
				return fmt.Errorf("%s is not a vault: %w", args[0], err)
			}
			absolute, err := filepath.Abs(found)
			if err != nil {
				return err
			}
			// A relative path saved here would mean something different from
			// every directory it is later read in.
			if err := userconfig.Save(userconfig.Settings{Vault: absolute}); err != nil {
				return err
			}
			return rt.Emit(&Response{
				Command: "use",
				Status:  StatusOK,
				Payload: UsePayload{Vault: absolute, Settings: settingsPath, Changed: true},
				Human:   humanUse,
			})
		},
	}
	cmd.Flags().BoolVar(&clear, "clear", false,
		"forget the saved vault, leaving kagaz to the working directory")
	return cmd
}

func humanUse(w io.Writer, payload any) error {
	p, ok := payload.(UsePayload)
	if !ok {
		return fmt.Errorf("unexpected payload %T", payload)
	}
	switch {
	case p.Vault == "" && p.Changed:
		fmt.Fprintf(w, "no vault saved; kagaz will use whatever the working directory is inside\n")
	case p.Vault == "":
		fmt.Fprintf(w, "no vault saved — kagaz uses whatever the working directory is inside\n")
		fmt.Fprintf(w, "set one with `kagaz use <path to vault.yaml>`\n")
	case p.Changed:
		fmt.Fprintf(w, "kagaz will use %s when no --vault is given\n", p.Vault)
	default:
		fmt.Fprintf(w, "%s\n", p.Vault)
	}
	fmt.Fprintf(w, "settings: %s\n", p.Settings)
	return nil
}
