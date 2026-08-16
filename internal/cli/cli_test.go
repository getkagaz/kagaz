package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func TestMCPStubIsPropseOnlyAndHasNoExecuteTool(t *testing.T) {
	out, _, code := run(t, "mcp", "--json")
	if code == ExitOK {
		t.Fatalf("the mcp stub reported success: %s", out)
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
