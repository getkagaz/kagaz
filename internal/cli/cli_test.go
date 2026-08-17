package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/getkagaz/kagaz/internal/vaultkit/config"
)

// run executes the CLI with args and returns stdout, stderr and the exit code.
func run(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	var out, errw bytes.Buffer
	code := Main("1.2.3", args, &out, &errw, strings.NewReader(""))
	return out.String(), errw.String(), code
}

// initDemo creates a demo vault in a temp directory and returns its vault.yaml.
func initDemo(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "vault")
	out, errw, code := run(t, "init", "--root", root, "--demo")
	if code != ExitOK {
		t.Fatalf("init --demo exit %d\nstdout: %s\nstderr: %s", code, out, errw)
	}
	vault := filepath.Join(root, "vault.yaml")
	if _, err := os.Stat(vault); err != nil {
		t.Fatalf("init --demo did not create %s: %v", vault, err)
	}
	return vault
}

// decode parses a JSON envelope.
func decode(t *testing.T, s string) map[string]any {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal([]byte(s), &obj); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, s)
	}
	return obj
}

func TestVersionFlagPrintsBareVersion(t *testing.T) {
	for _, args := range [][]string{{"--version"}, {"find", "--version"}} {
		out, _, code := run(t, args...)
		if code != ExitOK {
			t.Fatalf("%v: exit %d", args, code)
		}
		if strings.TrimSpace(out) != "1.2.3" {
			t.Fatalf("%v: want bare version, got %q", args, out)
		}
	}
}

func TestInitDemoCreatesVaultYAMLAtRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "v")
	if _, _, code := run(t, "init", "--root", root, "--demo"); code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	// The Homebrew formula asserts exactly this path.
	if _, err := os.Stat(filepath.Join(root, "vault.yaml")); err != nil {
		t.Fatalf("vault.yaml not at <root>/vault.yaml: %v", err)
	}
}

func TestVaultFlagAcceptsFileAndDirectory(t *testing.T) {
	vault := initDemo(t)
	for _, ref := range []string{vault, filepath.Dir(vault)} {
		out, errw, code := run(t, "--vault", ref, "find", "--json")
		if code != ExitOK {
			t.Fatalf("--vault %s: exit %d: %s", ref, code, errw)
		}
		if decode(t, out)["command"] != "find" {
			t.Fatalf("--vault %s: unexpected envelope: %s", ref, out)
		}
	}
}

func TestFindJSONOnDemoVaultIsNonEmpty(t *testing.T) {
	vault := initDemo(t)
	out, errw, code := run(t, "--vault", vault, "find", "--json")
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, errw)
	}
	obj := decode(t, out)
	if len(obj) == 0 {
		t.Fatal("find --json returned an empty JSON value")
	}
	count, ok := obj["count"].(float64)
	if !ok || count == 0 {
		t.Fatalf("demo vault returned no documents: %s", out)
	}
	results, ok := obj["results"].([]any)
	if !ok || len(results) == 0 {
		t.Fatalf("results missing or empty: %s", out)
	}
	first, ok := results[0].(map[string]any)
	if !ok {
		t.Fatalf("result is not an object: %s", out)
	}
	for _, key := range []string{"path", "rel_path", "name", "doctype", "tags", "parsed"} {
		if _, ok := first[key]; !ok {
			t.Errorf("find result is missing documented key %q", key)
		}
	}
	if obj["schema_version"].(float64) != float64(SchemaVersion) {
		t.Errorf("schema_version = %v", obj["schema_version"])
	}
}

func TestFindFiltersNarrowResults(t *testing.T) {
	vault := initDemo(t)
	all, _, _ := run(t, "--vault", vault, "find", "--json")
	filtered, _, code := run(t, "--vault", vault, "find", "--doctype", "passport", "--json")
	if code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	allCount := decode(t, all)["count"].(float64)
	someCount := decode(t, filtered)["count"].(float64)
	if someCount == 0 || someCount >= allCount {
		t.Fatalf("--doctype passport returned %v of %v documents", someCount, allCount)
	}
}

func TestLintIsCleanOnAFreshDemoVault(t *testing.T) {
	vault := initDemo(t)
	out, errw, code := run(t, "--vault", vault, "lint", "--json")
	if code != ExitOK {
		t.Fatalf("lint on a fresh demo vault exited %d\n%s\n%s", code, out, errw)
	}
	obj := decode(t, out)
	findings, _ := obj["findings"].([]any)
	if len(findings) != 0 {
		t.Fatalf("demo vault is not lint-clean: %s", out)
	}
}

func TestIndexGeneratesBothFiles(t *testing.T) {
	vault := initDemo(t)
	out, errw, code := run(t, "--vault", vault, "index", "--json")
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, errw)
	}
	written, _ := decode(t, out)["written"].([]any)
	if len(written) != 2 {
		t.Fatalf("index wrote %d files: %s", len(written), out)
	}
	// Generated files at the vault root must not become lint findings.
	if _, _, code := run(t, "--vault", vault, "lint", "--json"); code != ExitOK {
		t.Fatalf("lint exited %d after index", code)
	}
}

func TestDoctorDegradesWithNoOptionalToolsAndStaysZero(t *testing.T) {
	vault := initDemo(t)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("KAGAZ_MACHELPER", filepath.Join(t.TempDir(), "absent"))
	out, errw, code := run(t, "--vault", vault, "doctor", "--json")
	if code != ExitOK {
		t.Fatalf("doctor exited %d with every optional tool absent: %s\n%s", code, out, errw)
	}
	obj := decode(t, out)
	if obj["ok"] != true {
		t.Fatalf("doctor reported not-ok with only optional tools missing: %s", out)
	}
	checks, _ := obj["checks"].([]any)
	if len(checks) == 0 {
		t.Fatal("doctor ran no checks")
	}
	names := map[string]string{}
	for _, c := range checks {
		m := c.(map[string]any)
		names[m["name"].(string)] = m["status"].(string)
	}
	for _, want := range []string{"vault", "vault-root", "xattr", "spotlight", "pdftotext", "kagaz-machelper", "brctl", "ollama"} {
		if _, ok := names[want]; !ok {
			t.Errorf("doctor did not run the %q check (ran: %v)", want, names)
		}
	}
	for _, optional := range []string{"spotlight", "pdftotext", "kagaz-machelper", "brctl", "ollama"} {
		if names[optional] == CheckFail {
			t.Errorf("missing optional tool %q was reported as a failure", optional)
		}
	}
}

func TestDoctorFailsWithoutAVault(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(VaultEnv, filepath.Join(dir, "nowhere"))
	out, _, code := run(t, "--vault", filepath.Join(dir, "nowhere"), "doctor", "--json")
	if code == ExitOK {
		t.Fatalf("doctor without a vault exited 0: %s", out)
	}
	if decode(t, out)["ok"] != false {
		t.Fatalf("doctor without a vault reported ok: %s", out)
	}
}

func TestIngestProposeOnlyMutatesNothing(t *testing.T) {
	vault := initDemo(t)
	inbox := t.TempDir()
	src := filepath.Join(inbox, "acme invoice 2026.pdf")
	if err := os.WriteFile(src, renderPDF("Tax Invoice", []string{
		"ACME CORP", "TAX INVOICE", "Invoice Number: AC-2026-9001",
		"Bill To: Alex Rao", "Total: 120.00",
	}), 0o644); err != nil {
		t.Fatal(err)
	}

	out, errw, code := run(t, "--vault", vault, "ingest", "--propose-only", "--json", src)
	if code != ExitOK {
		t.Fatalf("--propose-only exited %d: %s\n%s", code, out, errw)
	}
	obj := decode(t, out)
	if obj["status"] != StatusProposed {
		t.Fatalf("status = %v, want %q: %s", obj["status"], StatusProposed, out)
	}
	if obj["executed"] != false {
		t.Fatalf("--propose-only reported executed: %s", out)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("--propose-only moved the source file: %v", err)
	}
	proposals, _ := obj["proposals"].([]any)
	if len(proposals) != 1 {
		t.Fatalf("want 1 proposal, got %d: %s", len(proposals), out)
	}
}

func TestIngestWithoutApprovalRefusesAndChangesNothing(t *testing.T) {
	vault := initDemo(t)
	src := filepath.Join(t.TempDir(), "note.pdf")
	if err := os.WriteFile(src, renderPDF("Receipt", []string{"Receipt Number: R-1", "Total: 9.00"}), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _, code := run(t, "--vault", vault, "ingest", "--json", src)
	if code != ExitConfirmationRequired {
		t.Fatalf("exit %d, want %d: %s", code, ExitConfirmationRequired, out)
	}
	if decode(t, out)["status"] != StatusConfirmationRequired {
		t.Fatalf("status: %s", out)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("the source file moved without approval: %v", err)
	}
}

func TestIngestExecutesWithSelection(t *testing.T) {
	vault := initDemo(t)
	src := filepath.Join(t.TempDir(), "globex receipt.pdf")
	if err := os.WriteFile(src, renderPDF("Receipt", []string{
		"GLOBEX RETAIL", "", "TRANSACTION RECEIPT", "Receipt Number: GX-99001",
		"Date: 02/02/2026", "Customer: Sam Rao", "", "Total: 41.00",
		"Payment received. Thank you for your payment.",
	}), 0o644); err != nil {
		t.Fatal(err)
	}
	out, errw, code := run(t, "--vault", vault, "ingest", "--select", "all", "--json", src)
	if code != ExitOK {
		t.Fatalf("exit %d: %s\n%s", code, out, errw)
	}
	obj := decode(t, out)
	if obj["executed"] != true {
		t.Fatalf("nothing executed: %s", out)
	}
	t.Logf("ingest response: %s", out)
	if _, err := os.Stat(src); err == nil {
		t.Fatal("the source is still in place after a successful ingest")
	}
	manifest, _ := obj["manifest"].(string)
	if manifest == "" {
		t.Fatalf("no manifest recorded: %s", out)
	}
	// Every mutation must be reversible.
	if _, errw, code := run(t, "--vault", vault, "rollback", "--yes", "--json", manifest); code != ExitOK {
		t.Fatalf("rollback exited %d: %s", code, errw)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("rollback did not restore the source: %v", err)
	}
}

func TestTagRequiresApprovalAndValidatesVocabulary(t *testing.T) {
	vault := initDemo(t)
	doc := firstDocument(t, vault)

	out, _, code := run(t, "--vault", vault, "tag", "--add", "not-a-real-tag", "--yes", doc)
	if code == ExitOK {
		t.Fatalf("an out-of-vocabulary tag was accepted: %s", out)
	}
	out, _, code = run(t, "--vault", vault, "tag", "--add", "to-action", "--json", doc)
	if code != ExitConfirmationRequired {
		t.Fatalf("tag without --yes exited %d: %s", code, out)
	}
}

func TestResolveForSendGateJSONNeverEmitsAPathWithoutConfirm(t *testing.T) {
	vault := initDemo(t)
	doc := confidentialDocument(t, vault)

	out, _, code := run(t, "--vault", vault, "resolve", "--for-send", "--json", doc)
	if code != ExitConfirmationRequired {
		t.Fatalf("gated resolve exited %d, want %d: %s", code, ExitConfirmationRequired, out)
	}
	obj := decode(t, out)
	if obj["status"] != StatusConfirmationRequired {
		t.Fatalf("status = %v: %s", obj["status"], out)
	}
	if obj["resolved_path"] != nil {
		t.Fatalf("a resolvable path was emitted without confirmation: %s", out)
	}
	if obj["message"] == nil || obj["reason"] == nil || obj["path"] == nil {
		t.Fatalf("confirmation_required response is missing documented keys: %s", out)
	}

	// The audit line is written on the refusal too.
	logOut, _, code := run(t, "--vault", vault, "log", "--json")
	if code != ExitOK {
		t.Fatalf("log exited %d", code)
	}
	if !strings.Contains(logOut, "resolve-for-send") {
		t.Fatalf("no audit line for the refused resolution: %s", logOut)
	}

	out, errw, code := run(t, "--vault", vault, "resolve", "--for-send", "--confirm", "--json", doc)
	if code != ExitOK {
		t.Fatalf("--confirm exited %d: %s\n%s", code, out, errw)
	}
	obj = decode(t, out)
	if obj["resolved_path"] != doc {
		t.Fatalf("resolved_path = %v, want %s", obj["resolved_path"], doc)
	}
	if obj["confirmed"] != true {
		t.Fatalf("confirmed = %v: %s", obj["confirmed"], out)
	}
}

func TestResolveWithoutForSendReturnsAPath(t *testing.T) {
	vault := initDemo(t)
	doc := firstDocument(t, vault)
	out, errw, code := run(t, "--vault", vault, "resolve", "--json", doc)
	if code != ExitOK {
		t.Fatalf("exit %d: %s\n%s", code, out, errw)
	}
	if decode(t, out)["resolved_path"] != doc {
		t.Fatalf("resolve did not return the document path: %s", out)
	}
}

func TestMCPSurfaceIsProposeOnlyAndHasNoExecuteTool(t *testing.T) {
	out, errw, code := run(t, "mcp", "--describe", "--json")
	if code != ExitOK {
		t.Fatalf("mcp --describe exit %d: %s\n%s", code, out, errw)
	}
	obj := decode(t, out)
	tools, _ := obj["tools"].([]any)
	if len(tools) == 0 {
		t.Fatalf("no tools declared: %s", out)
	}
	for _, tl := range tools {
		m := tl.(map[string]any)
		if m["mutates"] == true {
			t.Errorf("tool %v mutates; the MCP surface is propose-only", m["name"])
		}
		if name, _ := m["name"].(string); strings.Contains(name, "execute") {
			t.Errorf("an execute tool exists: %v", name)
		}
	}
}

func TestCompletionsGenerateForEveryShell(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish", "powershell"} {
		out, errw, code := run(t, "completion", shell)
		if code != ExitOK {
			t.Fatalf("completion %s exited %d: %s", shell, code, errw)
		}
		if len(out) < 100 {
			t.Fatalf("completion %s produced %d bytes", shell, len(out))
		}
	}
}

func TestUnknownFlagIsAUsageError(t *testing.T) {
	if _, _, code := run(t, "find", "--nope"); code != ExitUsage {
		t.Fatalf("exit %d, want %d", code, ExitUsage)
	}
}

func TestErrorsAreReportedAsAJSONEnvelope(t *testing.T) {
	dir := t.TempDir()
	var out, errw bytes.Buffer
	code := Main("1.2.3", []string{"--vault", filepath.Join(dir, "nope"), "find", "--json"}, &out, &errw, strings.NewReader(""))
	if code != ExitFailure {
		t.Fatalf("exit %d", code)
	}
	obj := decode(t, errw.String())
	if obj["status"] != StatusError || obj["message"] == nil {
		t.Fatalf("error envelope: %s", errw.String())
	}
}

// runStdin executes the CLI with the given stdin, so that tests can reproduce
// what launchd, cron and `brew services` actually hand a process.
func runStdin(t *testing.T, in io.Reader, args ...string) (string, string, int) {
	t.Helper()
	var out, errw bytes.Buffer
	code := Main("1.2.3", args, &out, &errw, in)
	return out.String(), errw.String(), code
}

// devNull opens /dev/null as a real *os.File, which is what makes it a
// character device and therefore the case os.ModeCharDevice alone gets wrong.
func devNull(t *testing.T) *os.File {
	t.Helper()
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Skipf("no %s on this platform: %v", os.DevNull, err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func TestDevNullStdinIsNotInteractive(t *testing.T) {
	rt := NewRuntime("1.2.3", io.Discard, io.Discard, devNull(t))
	if rt.Interactive() {
		t.Fatal("stdin redirected from /dev/null was reported as a terminal")
	}
	if _, err := rt.Prompt("q? "); err == nil {
		t.Fatal("Prompt succeeded on a non-terminal stdin")
	}
}

// TestMutatingCommandWithNullStdinIsDiagnosable is the launchd/cron/CI case:
// `kagaz tag ... < /dev/null` must refuse, say why, and exit non-zero. Exiting
// non-zero with no output at all leaves a scheduled job with nothing to report.
func TestMutatingCommandWithNullStdinIsDiagnosable(t *testing.T) {
	vault := initDemo(t)
	doc := firstDocument(t, vault)

	out, errw, code := runStdin(t, devNull(t), "--vault", vault, "tag", "--add", "to-action", doc)
	if code == ExitOK {
		t.Fatalf("a mutating command with no terminal exited 0: %s", out)
	}
	if strings.TrimSpace(out+errw) == "" {
		t.Fatalf("exit %d with no output whatsoever on stdout or stderr", code)
	}
	if !strings.Contains(out+errw, "--yes") {
		t.Fatalf("the message does not say how to proceed:\nstdout: %s\nstderr: %s", out, errw)
	}
	if _, err := os.Stat(doc); err != nil {
		t.Fatalf("the document moved: %v", err)
	}
}

func TestErrorResponseIsNeverSilent(t *testing.T) {
	var out, errw bytes.Buffer
	rt := NewRuntime("1.2.3", &out, &errw, strings.NewReader(""))
	err := rt.Emit(ErrorResponse("move", errors.New("something broke"), "try this instead", ExitFailure))
	if err == nil {
		t.Fatal("an error response exited zero")
	}
	if !strings.Contains(errw.String(), "something broke") || !strings.Contains(errw.String(), "try this instead") {
		t.Fatalf("error rendering lost the message: %q", errw.String())
	}
}

// TestDoctorStatusMatchesTheVerdict is the contract an agent branches on:
// `status` must never say ok while the process exits non-zero.
func TestDoctorStatusMatchesTheVerdict(t *testing.T) {
	t.Run("healthy vault", func(t *testing.T) {
		vault := initDemo(t)
		out, _, code := run(t, "--vault", vault, "doctor", "--json")
		if code != ExitOK {
			t.Skipf("this machine's doctor is not clean, nothing to assert here: %s", out)
		}
		if decode(t, out)["status"] != StatusOK {
			t.Fatalf("status on a healthy vault: %s", out)
		}
	})
	t.Run("broken vault", func(t *testing.T) {
		dir := t.TempDir()
		vault := filepath.Join(dir, "vault.yaml")
		if err := os.WriteFile(vault, []byte("version: 1\nvault_root: "+filepath.Join(dir, "gone")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		out, errw, code := run(t, "--vault", vault, "doctor", "--json")
		if code == ExitOK {
			t.Fatalf("doctor exited 0 against a vault whose root does not exist: %s\n%s", out, errw)
		}
		obj := decode(t, out+errw)
		if obj["status"] == StatusOK {
			t.Fatalf("doctor reported status=ok while exiting %d: %s", code, out+errw)
		}
		if obj["status"] != StatusError {
			t.Fatalf("status = %v, want %q: %s", obj["status"], StatusError, out+errw)
		}
		if obj["ok"] != false {
			t.Fatalf("ok = %v: %s", obj["ok"], out+errw)
		}
	})
	t.Run("human summary always states a verdict", func(t *testing.T) {
		var b bytes.Buffer
		payload := DoctorPayload{OK: false, Counts: map[string]int{CheckFail: 2}}
		if err := humanDoctor(&b, payload); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(b.String(), "core function is NOT available") {
			t.Fatalf("a failing doctor printed no verdict: %q", b.String())
		}
	})
}

// TestMoveResolvesRelativeDestinationsAgainstTheVaultFirst pins the resolution
// order: the vault root, then the working directory.
func TestMoveResolvesRelativeDestinationsAgainstTheVaultFirst(t *testing.T) {
	work := t.TempDir()
	vaultRoot := filepath.Join(work, "vault")
	cwdDir := filepath.Join(work, "cwd")
	for _, d := range []string{filepath.Join(vaultRoot, "Financial"), filepath.Join(cwdDir, "Financial")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Config{VaultRoot: vaultRoot}

	restore, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(cwdDir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(restore)
	// On macOS /var is a symlink to /private/var, so ask the OS what the
	// working directory is actually called rather than assuming.
	cwdDir, err = os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	// Both exist: the vault wins, because the vault is the subject of the
	// command. The reviewer's `kagaz move doc.pdf Financial` from ~ must not
	// file into ~/Financial.
	got, err := destinationPath(cfg, "Financial", "doc.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(vaultRoot, "Financial", "doc.pdf"); got != want {
		t.Fatalf("relative destination resolved to %s, want %s", got, want)
	}

	// Only the working directory has it: that is the fallback.
	if err := os.MkdirAll(filepath.Join(cwdDir, "Scratch"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err = destinationPath(cfg, "Scratch", "doc.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(cwdDir, "Scratch", "doc.pdf"); got != want {
		t.Fatalf("cwd fallback resolved to %s, want %s", got, want)
	}

	// Neither exists: a new path belongs in the vault.
	got, err = destinationPath(cfg, "Nowhere/x.pdf", "doc.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(vaultRoot, "Nowhere", "x.pdf"); got != want {
		t.Fatalf("unresolvable destination became %s, want %s", got, want)
	}
}

func TestMoveRefusesADestinationOutsideTheVault(t *testing.T) {
	vault := initDemo(t)
	doc := firstDocument(t, vault)
	outside := filepath.Join(t.TempDir(), "outside", "x.pdf")
	if err := os.MkdirAll(filepath.Dir(outside), 0o755); err != nil {
		t.Fatal(err)
	}

	out, errw, code := run(t, "--vault", vault, "move", "--yes", doc, outside)
	if code == ExitOK {
		t.Fatalf("a document was filed out of the vault without an opt-out: %s\n%s", out, errw)
	}
	if !strings.Contains(out+errw, "outside the vault") || !strings.Contains(out+errw, "--allow-outside-vault") {
		t.Fatalf("the refusal does not name the opt-out:\n%s\n%s", out, errw)
	}
	if _, err := os.Stat(doc); err != nil {
		t.Fatalf("the document moved despite the refusal: %v", err)
	}
	if _, err := os.Stat(outside); err == nil {
		t.Fatal("the destination outside the vault was written")
	}

	// The opt-out is a real opt-out, and it is reported in the payload.
	out, errw, code = run(t, "--vault", vault, "move", "--yes", "--allow-outside-vault", "--json", doc, outside)
	if code != ExitOK {
		t.Fatalf("--allow-outside-vault exited %d: %s\n%s", code, out, errw)
	}
	if decode(t, out)["outside_vault"] != true {
		t.Fatalf("the payload does not report the destination as outside the vault: %s", out)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("--allow-outside-vault did not move the document: %v", err)
	}
	var preview bytes.Buffer
	if err := previewMove(&preview, MovePayload{Outside: true, VaultRoot: "/v"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(preview.String(), "OUTSIDE THE VAULT") {
		t.Fatalf("the preview does not state that the destination is outside the vault: %q", preview.String())
	}
}

func TestUnknownCommandWithJSONProducesAnEnvelope(t *testing.T) {
	out, errw, code := run(t, "nosuchcommand", "--json")
	if code != ExitUsage {
		t.Fatalf("exit %d, want %d", code, ExitUsage)
	}
	obj := decode(t, out+errw)
	if obj["status"] != StatusError || obj["message"] == nil {
		t.Fatalf("unknown command did not produce an error envelope: %s%s", out, errw)
	}
}

func TestRollbackReportsSkippedRowsInThePayload(t *testing.T) {
	vault := initDemo(t)
	src := filepath.Join(t.TempDir(), "globex receipt.pdf")
	if err := os.WriteFile(src, renderPDF("Receipt", []string{
		"GLOBEX RETAIL", "", "TRANSACTION RECEIPT", "Receipt Number: GX-99002",
		"Date: 02/02/2026", "Customer: Sam Rao", "", "Total: 41.00",
		"Payment received. Thank you for your payment.",
	}), 0o644); err != nil {
		t.Fatal(err)
	}
	out, errw, code := run(t, "--vault", vault, "ingest", "--select", "all", "--json", src)
	if code != ExitOK {
		t.Fatalf("ingest exited %d: %s\n%s", code, out, errw)
	}
	manifest, _ := decode(t, out)["manifest"].(string)
	if manifest == "" {
		t.Fatalf("no manifest: %s", out)
	}
	if _, errw, code := run(t, "--vault", vault, "rollback", "--yes", "--json", manifest); code != ExitOK {
		t.Fatalf("first rollback exited %d: %s", code, errw)
	}
	// The second run reverses nothing. It must say so in the payload, not only
	// in a warning string that --json throws away.
	out, errw, code = run(t, "--vault", vault, "rollback", "--yes", "--json", manifest)
	if code != ExitOK {
		t.Fatalf("second rollback exited %d: %s\n%s", code, out, errw)
	}
	obj := decode(t, out)
	skipped, _ := obj["skipped"].([]any)
	if len(skipped) == 0 {
		t.Fatalf("re-running rollback reported no skipped rows: %s", out)
	}
	restored, _ := obj["restored"].([]any)
	if len(restored) != 0 {
		t.Fatalf("re-running rollback claimed to restore %d row(s): %s", len(restored), out)
	}
}

func TestIngestGuidanceNamesTheCauseAndAWorkingNextStep(t *testing.T) {
	vault := initDemo(t)
	// A format Kagaz genuinely has no reader for. The error must name the
	// format, not the machine: "no text extractor on this machine" reads as a
	// missing tool the user could install, and no install fixes this.
	src := filepath.Join(t.TempDir(), "random notes.xyz")
	if err := os.WriteFile(src, []byte("just some words\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, errw, code := run(t, "--vault", vault, "ingest", "--propose-only", "--json", src)
	if code != ExitOK {
		t.Fatalf("exit %d: %s\n%s", code, out, errw)
	}
	obj := decode(t, out)
	guidance, _ := obj["guidance"].([]any)
	if len(guidance) == 0 {
		t.Skipf("this machine classified the file, so there is no dead end to check: %s", out)
	}
	g := guidance[0].(map[string]any)
	next, _ := g["next_step"].(string)
	if !strings.Contains(next, "<destination>") {
		t.Fatalf("the suggested next step is the destination-less form that fails: %q", next)
	}
	cause, _ := g["cause"].(string)
	if !strings.Contains(cause, ".xyz") {
		t.Fatalf("the cause does not name the format Kagaz cannot read: %q", cause)
	}
	if strings.Contains(cause, "on this machine") {
		t.Fatalf("the cause blames the machine for a format no machine can read: %q", cause)
	}
	if got := tidyReasons("no text extracted: no text extracted"); got != "no text extracted" {
		t.Fatalf("doubled message not collapsed: %q", got)
	}
}

// TestIngestGuidanceDoesNotContradictItsOwnWarning: a `.xls` that is not an
// OLE2 container must not be answered with "run kagaz doctor".
//
// Because readableExt(".xls") is true, every `.xls` failure used to take the
// doctor branch, so the guidance said "no text could be extracted from this
// .xls (run kagaz doctor to see which extraction tiers are available)" directly
// beneath a warning that had already said the file is not a compound file.
// doctor would then report the legacyoffice tier present and working, and the
// user would go hunting for a tooling problem that does not exist. The standing
// rule is that an error never blames the machine for something no machine can
// fix, and a `.csv` named `.xls` is exactly that.
func TestIngestGuidanceDoesNotContradictItsOwnWarning(t *testing.T) {
	vault := initDemo(t)
	src := filepath.Join(t.TempDir(), "quarterly numbers.xls")
	if err := os.WriteFile(src, []byte("Date,Amount\n2024-03-12,1250.00\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, errw, code := run(t, "--vault", vault, "ingest", "--propose-only", "--json", src)
	if code != ExitOK {
		t.Fatalf("exit %d: %s\n%s", code, out, errw)
	}
	obj := decode(t, out)
	guidance, _ := obj["guidance"].([]any)
	if len(guidance) == 0 {
		t.Fatalf("a .xls that is not a compound file produced no guidance: %s", out)
	}
	cause, _ := guidance[0].(map[string]any)["cause"].(string)
	if strings.Contains(cause, "doctor") {
		t.Errorf("the guidance sends the user to doctor for a file that is simply not a .xls: %q", cause)
	}
	if !strings.Contains(cause, "OLE2") {
		t.Errorf("the guidance does not say what the file is not: %q", cause)
	}
}

// TestReadableFormatsMatchesReadableExt: the sentence the user is shown must
// list the formats readableExt actually accepts. It drifted once already --
// .rtfd and .wordml were readable but unlisted -- and a guidance string that
// quietly under-promises is a user re-saving a file they never had to.
func TestReadableFormatsMatchesReadableExt(t *testing.T) {
	for _, ext := range []string{".txt", ".md", ".docx", ".xlsx", ".pptx",
		".doc", ".rtf", ".rtfd", ".odt", ".wordml", ".xls", ".ppt"} {
		if !readableExt(ext) {
			t.Errorf("readableExt(%q) = false, but Kagaz has a tier for it", ext)
		}
		if !strings.Contains(readableFormats, ext) && !strings.Contains(readableFormats, "plain text") {
			t.Errorf("readableFormats does not mention %q: %q", ext, readableFormats)
		}
	}
	for _, ext := range []string{".doc", ".rtf", ".rtfd", ".odt", ".wordml", ".xls", ".ppt",
		".docx", ".xlsx", ".pptx"} {
		if !strings.Contains(readableFormats, ext) {
			t.Errorf("readableFormats omits %q, which readableExt accepts: %q", ext, readableFormats)
		}
	}
}

// TestIngestReadsPlainText: reading a .txt needs no tooling at all, and the
// project's own fixture vault is made of them, yet ingest used to refuse one
// with "no text extractor for .txt on this machine".
func TestIngestReadsPlainText(t *testing.T) {
	vault := initDemo(t)
	src := filepath.Join(t.TempDir(), "acme invoice.txt")
	body := "ACME CORP\n123 Industrial Way\n\nTAX INVOICE\n\nInvoice Number: INV-2024-0912\n" +
		"Date: 12/03/2024\nDue Date: 26/03/2024\n\nBill To:\nAlex Rao\n44 Sample Street\n\n" +
		"Description                 Qty     Rate      Amount\n" +
		"Consulting services           8    100.00     800.00\n\nTotal: 1,250.00\n\n" +
		"Payment is due within 14 days of the invoice date.\n"
	if err := os.WriteFile(src, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	out, errw, code := run(t, "--vault", vault, "ingest", "--propose-only", "--json", src)
	if code != ExitOK {
		t.Fatalf("exit %d: %s\n%s", code, out, errw)
	}
	obj := decode(t, out)
	proposals, _ := obj["proposals"].([]any)
	if len(proposals) != 1 {
		t.Fatalf("got %d proposals, want 1: %s", len(proposals), out)
	}
	p := proposals[0].(map[string]any)
	if engine, _ := p["ocr_engine"].(string); engine != "plaintext" {
		t.Errorf("ocr_engine = %q, want plaintext: %s", engine, out)
	}
	if doctype, _ := p["doctype"].(string); doctype != "invoice" {
		t.Errorf("doctype = %q, want invoice (the text was read, so rules can classify it)", doctype)
	}
	if skip, _ := p["skip"].(bool); skip {
		t.Errorf("a readable plain-text invoice was skipped: %s", out)
	}
}

// firstDocument returns the path of some document in the vault.
func firstDocument(t *testing.T, vault string) string {
	t.Helper()
	out, _, code := run(t, "--vault", vault, "find", "--json")
	if code != ExitOK {
		t.Fatalf("find exited %d", code)
	}
	results := decode(t, out)["results"].([]any)
	if len(results) == 0 {
		t.Fatal("no documents in the demo vault")
	}
	return results[0].(map[string]any)["path"].(string)
}

// confidentialDocument returns a document the confidential gate applies to. On
// a filesystem without extended attributes no document carries the tag, and
// the gate fails closed on every document instead — which is the behaviour
// being asserted either way.
func confidentialDocument(t *testing.T, vault string) string {
	t.Helper()
	out, _, code := run(t, "--vault", vault, "find", "--json")
	if code != ExitOK {
		t.Fatalf("find exited %d", code)
	}
	results := decode(t, out)["results"].([]any)
	for _, r := range results {
		m := r.(map[string]any)
		tagList, _ := m["tags"].([]any)
		for _, tg := range tagList {
			if tg == "confidential" {
				return m["path"].(string)
			}
		}
	}
	return firstDocument(t, vault)
}

// TestInitSeedsNoPeopleButDemoDoes covers the defect where a plain `kagaz init`
// wrote the two demo fixtures (Alex Rao, Sam Rao) into a real user's vault.
// A fresh vault must not assert that two people the user has never heard of
// exist -- owner inference would then match their documents against those
// names. The demo vault still needs them, so only --demo keeps them.
func TestInitSeedsNoPeopleButDemoDoes(t *testing.T) {
	plain := filepath.Join(t.TempDir(), "v")
	if _, errw, code := run(t, "init", "--root", plain); code != ExitOK {
		t.Fatalf("init exit %d: %s", code, errw)
	}
	cfg, err := config.LoadFile(filepath.Join(plain, "vault.yaml"))
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if len(cfg.People) != 0 {
		t.Fatalf("a plain init seeded %d people: %+v", len(cfg.People), cfg.People)
	}
	raw, err := os.ReadFile(filepath.Join(plain, "vault.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range demoPeople {
		if strings.Contains(string(raw), p.Name) {
			t.Fatalf("plain init's vault.yaml names the demo person %q", p.Name)
		}
	}

	demoCfg, err := config.LoadFile(initDemo(t))
	if err != nil {
		t.Fatalf("LoadFile(demo): %v", err)
	}
	if len(demoCfg.People) != len(demoPeople) {
		t.Fatalf("demo vault has %d people, want %d", len(demoCfg.People), len(demoPeople))
	}
}

// TestInitWritesAnEditableStructureBlock asserts that the mechanism the
// shared-folder error message points at is actually present in the file the
// user is told to edit: "add `shared: _Shared` under `structure.company`" is
// unfollowable when vault.yaml has no structure block.
func TestInitWritesAnEditableStructureBlock(t *testing.T) {
	root := filepath.Join(t.TempDir(), "v")
	if _, errw, code := run(t, "init", "--root", root); code != ExitOK {
		t.Fatalf("init exit %d: %s", code, errw)
	}
	raw, err := os.ReadFile(filepath.Join(root, "vault.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "\nstructure:\n") {
		t.Fatalf("init wrote no structure block:\n%s", raw)
	}
	cfg, err := config.LoadFile(filepath.Join(root, "vault.yaml"))
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	want := config.DefaultStructure()
	if len(cfg.Structure) != len(want) {
		t.Fatalf("init wrote %d categories, defaults have %d", len(cfg.Structure), len(want))
	}
	for name, cat := range want {
		got, ok := cfg.Structure[name]
		if !ok {
			t.Errorf("init omitted category %q", name)
			continue
		}
		if got != cat {
			t.Errorf("structure.%s = %+v, want %+v", name, got, cat)
		}
	}
}

// TestIngestProposesForAnUnownedDocumentInAFreshVault is the end-to-end
// regression test for the blocker: against a vault made by a plain `kagaz
// init`, every document with no owner to infer was skipped with "category
// %q defines no shared folder", which made a new user's first ingest file
// nothing at all.
func TestIngestProposesForAnUnownedDocumentInAFreshVault(t *testing.T) {
	root := filepath.Join(t.TempDir(), "v")
	if _, errw, code := run(t, "init", "--root", root); code != ExitOK {
		t.Fatalf("init exit %d: %s", code, errw)
	}
	vault := filepath.Join(root, "vault.yaml")

	src := filepath.Join(t.TempDir(), "northwind invoice 2026.pdf")
	if err := os.WriteFile(src, renderPDF("Tax Invoice", []string{
		"NORTHWIND HOLDINGS LIMITED",
		"TAX INVOICE",
		"Invoice Number: NH-2026-0042",
		"Invoice Date: 14/02/2026",
		"Consulting services ............ 1,200.00",
		"Total: 1200.00",
	}), 0o644); err != nil {
		t.Fatal(err)
	}

	out, errw, code := run(t, "--vault", vault, "ingest", "--propose-only", "--json", src)
	if code != ExitOK {
		t.Fatalf("ingest exit %d: %s\n%s", code, out, errw)
	}
	obj := decode(t, out)
	if strings.Contains(out, "defines no shared folder") {
		t.Fatalf("the shared-folder skip is back: %s", out)
	}
	proposals, _ := obj["proposals"].([]any)
	if len(proposals) != 1 {
		t.Fatalf("an unowned document produced %d proposals, want 1: %s", len(proposals), out)
	}
	dest, _ := proposals[0].(map[string]any)["dest"].(string)
	if dest == "" {
		t.Fatalf("proposal has no destination: %s", out)
	}
	if !strings.Contains(dest, config.DefaultSharedFolder) {
		t.Fatalf("destination %q does not use the shared folder", dest)
	}
}
