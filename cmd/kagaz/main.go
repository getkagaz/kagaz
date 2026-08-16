// Command kagaz is the Kagaz command-line interface: the only thing in the
// project that mutates a vault. The menu-bar app and the MCP server both shell
// out to this binary with --json rather than holding vault logic of their own.
package main

import (
	"os"

	"github.com/getkagaz/kagaz/internal/cli"
)

// version is the build version. The Homebrew formula injects it with
// -ldflags "-X main.version=<version>"; a build without it reports "dev".
var version string

func main() {
	if version == "" {
		version = "dev"
	}
	os.Exit(cli.Main(version, os.Args[1:], os.Stdout, os.Stderr, os.Stdin))
}
