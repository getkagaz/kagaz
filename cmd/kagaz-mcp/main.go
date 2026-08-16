// Command kagaz-mcp is the stdio MCP server wrapper around the kagaz CLI.
//
// The surface it will expose is propose-only: find, ingest_propose, tag and
// resolve_for_send. There is deliberately no execute tool — an agent proposes
// and a human approves — and resolve_for_send cannot auto-confirm the
// confidential gate, returning the same confirmation_required structure the
// CLI does.
//
// The server itself is not implemented in this build. What is implemented here
// is the contract every other piece already depends on: --version printing the
// bare version string, and a non-zero exit with an honest message rather than a
// binary that silently does nothing.
package main

import (
	"encoding/json"
	"fmt"
	"os"
)

// version is the build version, injected by the Homebrew formula with
// -ldflags "-X main.version=<version>".
var version string

// tool describes one planned MCP tool.
type tool struct {
	Name        string `json:"name"`
	Mutates     bool   `json:"mutates"`
	Description string `json:"description"`
}

var tools = []tool{
	{Name: "find", Description: "query the vault; returns the same shape as `kagaz find --json`"},
	{Name: "ingest_propose", Description: "analyse paths and return proposals; never files anything"},
	{Name: "tag", Description: "return the tag change `kagaz tag` would make"},
	{Name: "resolve_for_send", Description: "resolve for external send; cannot auto-confirm the confidential gate"},
}

func main() {
	if version == "" {
		version = "dev"
	}

	jsonOut := false
	for _, arg := range os.Args[1:] {
		switch arg {
		case "--version", "-v":
			fmt.Println(version)
			return
		case "--json":
			jsonOut = true
		case "--help", "-h":
			usage()
			return
		}
	}

	if jsonOut {
		out := map[string]any{
			"command":        "kagaz-mcp",
			"status":         "error",
			"schema_version": 1,
			"server_status":  "not-implemented",
			"transport":      "stdio JSON-RPC 2.0",
			"tools":          tools,
			"message":        "the MCP server is not implemented in this build; call `kagaz --json` directly",
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(1)
	}

	usage()
	os.Exit(1)
}

func usage() {
	fmt.Fprintf(os.Stderr, "kagaz-mcp %s\n\n", version)
	fmt.Fprintln(os.Stderr, "The stdio MCP server is not implemented in this build.")
	fmt.Fprintln(os.Stderr, "Planned tools, all propose-only (no execute tool exists):")
	for _, t := range tools {
		fmt.Fprintf(os.Stderr, "  %-18s %s\n", t.Name, t.Description)
	}
	fmt.Fprintln(os.Stderr, "\nUntil then, call `kagaz --json` directly.")
}
