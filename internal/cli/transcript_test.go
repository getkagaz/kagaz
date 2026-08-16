package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEndToEndTranscript drives the whole command surface against a real demo
// vault and writes a transcript to the file named by KAGAZ_TRANSCRIPT.
//
// It is skipped unless that variable is set: its purpose is producing a
// reviewable record of what the CLI actually prints, not asserting behaviour —
// the tests in cli_test.go do that. Every command below runs through exactly
// the entry point the binary's main() calls, so the transcript is what a user
// sees, not a reconstruction of it.
func TestEndToEndTranscript(t *testing.T) {
	dest := os.Getenv("KAGAZ_TRANSCRIPT")
	if dest == "" {
		t.Skip("set KAGAZ_TRANSCRIPT=<path> to record a transcript")
	}

	work := t.TempDir()
	vaultRoot := filepath.Join(work, "vault")
	vault := filepath.Join(vaultRoot, "vault.yaml")
	inbox := filepath.Join(work, "inbox")
	if err := os.MkdirAll(inbox, 0o755); err != nil {
		t.Fatal(err)
	}

	var log bytes.Buffer
	execIn := func(stdin string, args ...string) int {
		var out, errw bytes.Buffer
		code := Main("0.1.0", args, &out, &errw, strings.NewReader(stdin))
		if stdin != "" {
			fmt.Fprintf(&log, "\n$ printf '%%s\\n' '...' | kagaz %s\n%s", strings.Join(args, " "), stdin)
		} else {
			fmt.Fprintf(&log, "\n$ kagaz %s\n", strings.Join(args, " "))
		}
		if s := out.String(); s != "" {
			log.WriteString(s)
		}
		if s := errw.String(); s != "" {
			log.WriteString(s)
		}
		fmt.Fprintf(&log, "[exit %d]\n", code)
		return code
	}
	exec := func(args ...string) int { return execIn("", args...) }

	exec("--version")
	exec("init", "--root", vaultRoot, "--demo")
	exec("--vault", vault, "find")
	exec("--vault", vault, "find", "--person", "alex-rao", "--period", "FY2026")
	exec("--vault", vault, "find", "--doctype", "passport", "--json")
	exec("--vault", vault, "lint")
	exec("--vault", vault, "doctor")
	exec("--vault", vault, "index")
	exec("--vault", vault, "lint")

	loose := filepath.Join(inbox, "scan 0042.pdf")
	src := filepath.Join(vaultRoot, "Financial", "Alex-Rao", "FY 2026", "Invoice_Alex-Rao_Acme-Corp_2026.pdf")
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(loose, data, 0o644); err != nil {
		t.Fatal(err)
	}

	exec("--vault", vault, "ingest", "--propose-only", loose)
	exec("--vault", vault, "ingest", "--json", loose)
	exec("--vault", vault, "ingest", "--select", "all", loose)
	exec("--vault", vault, "log", "-n", "3")

	confidential := filepath.Join(vaultRoot, "Identity", "Alex-Rao", "Passport_Alex-Rao_Passport-Office_2024.pdf")
	exec("--vault", vault, "resolve", "--for-send", "--json", confidential)
	exec("--vault", vault, "resolve", "--for-send", "--confirm", "--json", confidential)
	boarding := filepath.Join(vaultRoot, "Travel", "Sam-Rao", "Boarding-Pass_Sam-Rao_United-Airlines_2026.pdf")
	exec("--vault", vault, "resolve", boarding)
	exec("--vault", vault, "log", "-n", "2")

	exec("--vault", vault, "tag", "--add", "to-action", "--json", boarding)
	exec("--vault", vault, "tag", "--add", "to-action", "--yes", boarding)
	exec("--vault", vault, "supersede", "--yes",
		filepath.Join(vaultRoot, "Identity", "Alex-Rao", "Passport_Alex-Rao_Passport-Office_2014.pdf"), confidential)
	exec("--vault", vault, "rollback")
	exec("--vault", vault, "lint")
	exec("mcp", "--describe")
	execIn(strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"transcript","version":"0"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"find","arguments":{"doctype":"passport"}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"resolve_for_send","arguments":{"reference":` + quote(confidential) + `}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"resolve_for_send","arguments":{"reference":` + quote(confidential) + `,"confirm":true}}}`,
		"",
	}, "\n"), "--vault", vault, "mcp")
	exec("--vault", vault, "find", "--nope")

	if body, err := os.ReadFile(vault); err == nil {
		fmt.Fprintf(&log, "\n$ cat vault.yaml\n%s\n", body)
	}
	if body, err := os.ReadFile(filepath.Join(vaultRoot, "INDEX.md")); err == nil {
		fmt.Fprintf(&log, "\n$ head INDEX.md\n%s\n", firstLines(string(body), 45))
	}
	var tree []string
	_ = filepath.Walk(vaultRoot, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			rel, _ := filepath.Rel(vaultRoot, p)
			tree = append(tree, rel)
		}
		return nil
	})
	fmt.Fprintf(&log, "\n$ find vault -type f\n%s\n", strings.Join(tree, "\n"))

	if err := os.WriteFile(dest, log.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("transcript written to %s (%d bytes)", dest, log.Len())
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
