// Command kagaz-mcp is the stdio MCP server: `kagaz mcp` under its own name,
// because an MCP client configuration names a binary, not a subcommand.
//
// It holds no logic of its own. Every argument is forwarded to the `mcp`
// subcommand of the very same CLI entry point cmd/kagaz calls, so the two
// spellings cannot drift: `kagaz-mcp` and `kagaz mcp` are one implementation.
//
// The surface is propose-only: find, ingest_propose, tag and resolve_for_send.
// There is deliberately no execute tool — an agent proposes and a human
// approves — and resolve_for_send cannot auto-confirm the confidential gate,
// returning the same confirmation_required structure the CLI does.
package main

import (
	"os"

	"github.com/getkagaz/kagaz/internal/cli"
)

// version is the build version, injected by the Homebrew formula with
// -ldflags "-X main.version=<version>".
var version string

func main() {
	if version == "" {
		version = "dev"
	}
	// "mcp" first, then the caller's arguments: --version, --help, --vault and
	// --describe are all the CLI's own, handled by the one command tree.
	args := append([]string{"mcp"}, os.Args[1:]...)
	os.Exit(cli.Main(version, args, os.Stdout, os.Stderr, os.Stdin))
}
