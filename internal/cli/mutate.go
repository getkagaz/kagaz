package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

// mutationFlags are the approval flags every mutating command carries. They
// implement the propose → preview → approve → execute rule in one place, so no
// command can accidentally acquire a silent path to mutation.
type mutationFlags struct {
	// yes approves the proposal without asking. --accept-proposal is the
	// agent-facing spelling of the same thing.
	yes bool
	// proposeOnly prints the proposal and exits 0 without mutating.
	proposeOnly bool
}

// register adds the approval flags to a command.
func (m *mutationFlags) register(cmd *cobra.Command) {
	f := cmd.Flags()
	f.BoolVarP(&m.yes, "yes", "y", false, "approve the proposal without prompting")
	f.BoolVar(&m.yes, "accept-proposal", false, "approve the proposal without prompting (alias of --yes)")
	f.BoolVar(&m.proposeOnly, "propose-only", false, "print the proposal and exit without mutating")
}

// approve shows the proposal and decides whether to proceed.
//
// It returns (true, nil) to execute, or (false, response) with the response the
// command must emit instead. The preview renderer receives the very same
// payload value the response carries, so what a user approves and what a JSON
// consumer sees are one description of one plan.
func (m *mutationFlags) approve(rt *Runtime, command string, payload any, preview func(io.Writer, any) error) (bool, *Response) {
	proposed := &Response{Command: command, Status: StatusProposed, Payload: payload, Human: preview}

	if m.proposeOnly {
		return false, proposed
	}
	if m.yes {
		return true, nil
	}
	if rt.JSON || !rt.Interactive() {
		// Prompting is impossible: a JSON caller has no terminal to answer on,
		// and neither does a pipeline. Refusing is the only honest answer —
		// assuming consent here would be exactly the silent mutation the whole
		// design exists to prevent.
		return false, &Response{
			Command: command,
			Status:  StatusConfirmationRequired,
			Payload: payload,
			Human:   preview,
			Exit:    ExitConfirmationRequired,
			Warnings: []string{
				"nothing was changed: re-run with --yes (or --accept-proposal) to approve this proposal",
			},
		}
	}

	if preview != nil {
		if err := preview(rt.Out, payload); err != nil {
			return false, ErrorResponse(command, err, "", ExitFailure)
		}
	}
	ok, err := rt.Confirm("\nApply this proposal?")
	if err != nil {
		return false, ErrorResponse(command, err, "", ExitFailure)
	}
	if !ok {
		fmt.Fprintln(rt.Out, "nothing was changed")
		return false, &Response{Command: command, Status: StatusProposed, Payload: payload}
	}
	return true, nil
}
