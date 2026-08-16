package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// newCompletionCommand wires up cobra's generated shell completions. They are
// free, so not shipping them would be a choice rather than an omission.
func newCompletionCommand(rt *Runtime) *cobra.Command {
	return &cobra.Command{
		Use:   "completion <bash|zsh|fish|powershell>",
		Short: "Generate a shell completion script",
		Long: "completion writes a shell completion script to stdout.\n\n" +
			"  kagaz completion zsh  > ~/.zsh/completions/_kagaz\n" +
			"  kagaz completion bash > /opt/homebrew/etc/bash_completion.d/kagaz\n" +
			"  kagaz completion fish > ~/.config/fish/completions/kagaz.fish\n\n" +
			"Installing kagaz with Homebrew wires these up for you.",
		Args:                  cobra.ExactArgs(1),
		ValidArgs:             []string{"bash", "zsh", "fish", "powershell"},
		DisableFlagsInUseLine: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			root := cmd.Root()
			switch args[0] {
			case "bash":
				return root.GenBashCompletionV2(rt.Out, true)
			case "zsh":
				return root.GenZshCompletion(rt.Out)
			case "fish":
				return root.GenFishCompletion(rt.Out, true)
			case "powershell":
				return root.GenPowerShellCompletionWithDesc(rt.Out)
			default:
				return fmt.Errorf("unsupported shell %q: use bash, zsh, fish or powershell", args[0])
			}
		},
	}
}
