package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

// MCPPayload is the `kagaz mcp --json` body. The tool list is declared here
// because it is a contract, not an implementation detail: the surface stays
// propose-only, and nothing in it executes a mutation.
type MCPPayload struct {
	// Status is "not-implemented" until the server lands.
	Status string `json:"server_status"`
	// Transport is the wire protocol the server will speak.
	Transport string `json:"transport"`
	// Tools are the tools the server exposes.
	Tools []MCPTool `json:"tools"`
	// Message explains the current state in one line.
	Message string `json:"message"`
}

// MCPTool describes one MCP tool.
type MCPTool struct {
	Name string `json:"name"`
	// Mutates is false for every tool in this surface, by design: an agent
	// proposes and a human executes. There is no execute tool.
	Mutates     bool   `json:"mutates"`
	Description string `json:"description"`
}

// mcpTools is the propose-only surface.
var mcpTools = []MCPTool{
	{Name: "find", Mutates: false, Description: "query the vault; returns the same shape as `kagaz find --json`"},
	{Name: "ingest_propose", Mutates: false, Description: "analyse paths and return proposals; never files anything"},
	{Name: "tag", Mutates: false, Description: "return the tag change that `kagaz tag` would make"},
	{Name: "resolve_for_send", Mutates: false, Description: "resolve a document for external send; cannot auto-confirm the confidential gate"},
}

func newMCPCommand(rt *Runtime) *cobra.Command {
	return &cobra.Command{
		Use:   "mcp",
		Short: "Start the stdio MCP server (not implemented in this build)",
		Long: "mcp starts the stdio MCP server. The surface is propose-only: an agent can\n" +
			"query, propose and ask for a resolution, and a human executes. There is no\n" +
			"execute tool, and resolve_for_send cannot auto-confirm the confidential gate.\n\n" +
			"The server itself is not implemented in this build; `kagaz-mcp` and this\n" +
			"command will share one implementation.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return rt.Emit(&Response{
				Command: "mcp",
				Status:  StatusError,
				Payload: MCPPayload{
					Status:    "not-implemented",
					Transport: "stdio JSON-RPC 2.0",
					Tools:     mcpTools,
					Message:   "the MCP server is not implemented in this build; use `kagaz --json` directly",
				},
				Human: humanMCP,
				Exit:  ExitFailure,
			})
		},
	}
}

func humanMCP(w io.Writer, payload any) error {
	p, ok := payload.(MCPPayload)
	if !ok {
		return fmt.Errorf("mcp: unexpected payload %T", payload)
	}
	fmt.Fprintf(w, "%s\n\nPlanned tools (%s), all propose-only:\n", p.Message, p.Transport)
	for _, t := range p.Tools {
		fmt.Fprintf(w, "  %-18s %s\n", t.Name, t.Description)
	}
	return nil
}
