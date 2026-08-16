package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/getkagaz/kagaz/internal/mcp"
	"github.com/getkagaz/kagaz/internal/vaultkit/audit"
	"github.com/getkagaz/kagaz/internal/vaultkit/config"
	"github.com/getkagaz/kagaz/internal/vaultkit/tags"
)

// fixtureVault copies testdata/fixture-vault into a temp directory and returns
// the copy's vault.yaml.
//
// A copy, not the fixture itself: resolve --for-send writes an audit line, and
// a test that mutates the repository's own fixture is a test that changes the
// next test's input.
func fixtureVault(t *testing.T) string {
	t.Helper()
	src, err := filepath.Abs(filepath.Join("..", "..", "testdata", "fixture-vault"))
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "vault")
	if err := copyTree(src, dst); err != nil {
		t.Fatalf("copy fixture vault: %v", err)
	}
	return filepath.Join(dst, "vault.yaml")
}

// copyTree copies a directory tree.
func copyTree(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}

// mcpSession runs `kagaz mcp` over the given request lines and returns the
// parsed responses, plus whatever went to stderr.
func mcpSession(t *testing.T, vault string, requests ...string) ([]map[string]any, string) {
	t.Helper()
	var out, errw bytes.Buffer
	args := []string{}
	if vault != "" {
		args = append(args, "--vault", vault)
	}
	args = append(args, "mcp")
	in := strings.NewReader(strings.Join(requests, "\n") + "\n")

	if code := Main("1.2.3", args, &out, &errw, in); code != ExitOK {
		t.Fatalf("mcp exited %d\nstdout: %s\nstderr: %s", code, out.String(), errw.String())
	}
	var msgs []map[string]any
	for _, line := range strings.Split(strings.TrimRight(out.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("stdout carried a non-protocol line: %v\n%s", err, line)
		}
		msgs = append(msgs, m)
	}
	return msgs, errw.String()
}

// callTool builds a tools/call request line.
func callTool(id int, name, arguments string) string {
	return `{"jsonrpc":"2.0","id":` + itoa(id) + `,"method":"tools/call","params":{"name":"` + name + `","arguments":` + arguments + `}}`
}

func itoa(i int) string {
	return string(rune('0' + i%10))
}

// toolEnvelope returns the JSON envelope a tool result carries, and whether the
// result was marked as an error.
func toolEnvelope(t *testing.T, msg map[string]any) (map[string]any, bool) {
	t.Helper()
	res, ok := msg["result"].(map[string]any)
	if !ok {
		t.Fatalf("not a tool result: %+v", msg)
	}
	content := res["content"].([]any)
	text := content[0].(map[string]any)["text"].(string)
	var env map[string]any
	if err := json.Unmarshal([]byte(text), &env); err != nil {
		t.Fatalf("tool text is not the JSON envelope: %v\n%s", err, text)
	}
	return env, res["isError"] == true
}

// gate makes doc subject to the confidential gate, whichever way this
// filesystem behaves: with extended attributes the tag is what gates it, and
// without them the gate fails closed on every document instead (which is the
// behaviour being relied on either way).
func gate(t *testing.T, doc string) {
	t.Helper()
	if err := tags.Add(doc, "confidential"); err != nil && !errors.Is(err, tags.ErrUnsupported) {
		t.Fatalf("tag %s confidential: %v", doc, err)
	}
}

// auditEntries reads the vault's audit log.
func auditEntries(t *testing.T, vault string) []audit.Entry {
	t.Helper()
	cfg, err := config.LoadFile(vault)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := audit.Open(cfg.AuditLogPath()).Tail(100)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	return entries
}

// resolveForSend calls the resolve_for_send tool once.
func resolveForSend(t *testing.T, vault, doc string, confirm bool) (map[string]any, bool) {
	t.Helper()
	args := `{"reference":` + quote(doc) + `}`
	if confirm {
		args = `{"reference":` + quote(doc) + `,"confirm":true}`
	}
	msgs, _ := mcpSession(t, vault, callTool(1, "resolve_for_send", args))
	if len(msgs) != 1 {
		t.Fatalf("got %d responses, want 1: %+v", len(msgs), msgs)
	}
	return toolEnvelope(t, msgs[0])
}

// quote renders a string as a JSON string literal.
func quote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func TestMCPToolsListDescribesTheFixedSurface(t *testing.T) {
	msgs, _ := mcpSession(t, "", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	tools := msgs[0]["result"].(map[string]any)["tools"].([]any)

	var names []string
	for _, tl := range tools {
		m := tl.(map[string]any)
		name := m["name"].(string)
		names = append(names, name)

		desc, _ := m["description"].(string)
		if len(desc) < 40 {
			t.Errorf("%s: the schema is the agent's only documentation; %q is not it", name, desc)
		}
		schema, ok := m["inputSchema"].(map[string]any)
		if !ok || schema["type"] != "object" {
			t.Errorf("%s: inputSchema is not a JSON object schema: %+v", name, m["inputSchema"])
		}
		if strings.Contains(name, "execute") || strings.Contains(name, "apply") {
			t.Errorf("%s: the MCP surface has no tool that executes a proposal", name)
		}
	}
	want := []string{"find", "ingest_propose", "tag", "resolve_for_send"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("tool surface = %v, want exactly %v", names, want)
	}

	// resolve_for_send's description must say what it costs to call it.
	for _, tl := range tools {
		m := tl.(map[string]any)
		if m["name"] != "resolve_for_send" {
			continue
		}
		desc := strings.ToLower(m["description"].(string))
		for _, phrase := range []string{"confirm", "audit"} {
			if !strings.Contains(desc, phrase) {
				t.Errorf("resolve_for_send's description does not mention %q: %s", phrase, desc)
			}
		}
	}
}

func TestMCPFindReturnsTheCLIEnvelope(t *testing.T) {
	vault := fixtureVault(t)
	msgs, _ := mcpSession(t, vault, callTool(1, "find", `{"doctype":"passport"}`))
	env, isErr := toolEnvelope(t, msgs[0])
	if isErr {
		t.Fatalf("find reported an error: %+v", env)
	}
	if env["command"] != "find" || env["status"] != "ok" || env["schema_version"] != float64(SchemaVersion) {
		t.Fatalf("not the find envelope: %+v", env)
	}
	results := env["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("got %d results, want the fixture's one passport: %+v", len(results), results)
	}
	if results[0].(map[string]any)["doctype"] != "passport" {
		t.Fatalf("unexpected result: %+v", results[0])
	}

	// The same query through the CLI must produce the same bytes: one data
	// path, two callers.
	cliOut, _, code := run(t, "--vault", vault, "find", "--json", "--doctype", "passport")
	if code != ExitOK {
		t.Fatalf("cli find exited %d", code)
	}
	var cliEnv map[string]any
	if err := json.Unmarshal([]byte(cliOut), &cliEnv); err != nil {
		t.Fatal(err)
	}
	if a, b := mustJSON(t, env["results"]), mustJSON(t, cliEnv["results"]); a != b {
		t.Fatalf("MCP and CLI disagree:\nMCP: %s\nCLI: %s", a, b)
	}
}

// mustJSON re-marshals a decoded value for comparison.
func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestMCPIngestProposeProposesAndFilesNothing(t *testing.T) {
	vault := fixtureVault(t)
	loose := filepath.Join(t.TempDir(), "scan 0042.txt")
	if err := os.WriteFile(loose, []byte("Invoice from Acme Corp for Alex Rao, 2026\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	msgs, _ := mcpSession(t, vault, callTool(1, "ingest_propose", `{"paths":[`+quote(loose)+`]}`))
	env, _ := toolEnvelope(t, msgs[0])
	if env["command"] != "ingest" || env["status"] != StatusProposed {
		t.Fatalf("not a proposal envelope: %+v", env)
	}
	if env["executed"] != false {
		t.Fatalf("ingest_propose executed something: %+v", env)
	}
	if len(env["proposals"].([]any)) != 1 {
		t.Fatalf("want one proposal per path: %+v", env["proposals"])
	}
	if _, err := os.Stat(loose); err != nil {
		t.Fatalf("the source file was moved by a propose-only tool: %v", err)
	}
	if env["manifest"] != nil {
		t.Fatalf("a propose-only call wrote a manifest: %+v", env["manifest"])
	}
}

func TestMCPTagProposesTheChangeAndWritesNoTag(t *testing.T) {
	vault := fixtureVault(t)
	doc := filepath.Join(filepath.Dir(vault), "Travel", "Sam-Rao", "Boarding-Pass_Sam-Rao_United-Airlines.txt")

	msgs, _ := mcpSession(t, vault, callTool(1, "tag", `{"paths":[`+quote(doc)+`],"add":["to-action"]}`))
	env, _ := toolEnvelope(t, msgs[0])
	if env["command"] != "tag" || env["status"] != StatusProposed {
		t.Fatalf("not a tag proposal: %+v", env)
	}
	change := env["changes"].([]any)[0].(map[string]any)
	if change["path"] != doc {
		t.Fatalf("change is about %v, want %s", change["path"], doc)
	}
	after := mustJSON(t, change["after"])
	if !strings.Contains(after, "to-action") {
		t.Fatalf("the proposed tag set does not contain the added tag: %s", after)
	}

	// Nothing was applied.
	now, err := tags.Read(doc)
	if err != nil && !errors.Is(err, tags.ErrUnsupported) {
		t.Fatal(err)
	}
	for _, tg := range now {
		if tg == "to-action" {
			t.Fatal("the tag tool wrote a tag; it is propose-only")
		}
	}
}

// TestMCPResolveForSendCannotBypassConfirmation is safety invariant 7 as an
// MCP call: no argument, and no default, hands back a path for a gated
// document without explicit confirmation, and both branches are audited.
func TestMCPResolveForSendCannotBypassConfirmation(t *testing.T) {
	vault := fixtureVault(t)
	doc := filepath.Join(filepath.Dir(vault), "Identity", "Alex-Rao", "Passport_Alex-Rao_Passport-Office_2024.txt")
	gate(t, doc)

	env, isErr := resolveForSend(t, vault, doc, false)
	if env["status"] != StatusConfirmationRequired {
		t.Fatalf("status = %v, want %s: %+v", env["status"], StatusConfirmationRequired, env)
	}
	if !isErr {
		t.Error("a refusal was reported to the model as a successful call")
	}
	if env["resolved_path"] != nil {
		t.Fatalf("a path was handed over without confirmation: %v", env["resolved_path"])
	}
	if env["confirmed"] != false || env["gated"] != true || env["materialized"] != false {
		t.Fatalf("the refusal does not describe itself honestly: %+v", env)
	}

	refused := auditEntries(t, vault)
	if len(refused) != 1 {
		t.Fatalf("the refusal wrote %d audit lines, want exactly 1: %+v", len(refused), refused)
	}
	if refused[0].Op != "resolve-for-send" || refused[0].Confirmed {
		t.Fatalf("the refusal's audit line is wrong: %+v", refused[0])
	}
	if refused[0].Detail["outcome"] != "not confirmed" {
		t.Fatalf("the refusal is not recorded as such: %+v", refused[0].Detail)
	}

	// Explicit consent, and only then, produces a path — and a second line.
	env, isErr = resolveForSend(t, vault, doc, true)
	if env["status"] != StatusOK || isErr {
		t.Fatalf("a confirmed resolution failed: %+v", env)
	}
	if env["resolved_path"] != doc {
		t.Fatalf("resolved_path = %v, want %s", env["resolved_path"], doc)
	}
	if env["confirmed"] != true || env["materialized"] != true {
		t.Fatalf("the approval does not describe itself honestly: %+v", env)
	}
	approved := auditEntries(t, vault)
	if len(approved) != 2 {
		t.Fatalf("the approval did not add an audit line: %+v", approved)
	}
	if !approved[1].Confirmed || approved[1].Op != "resolve-for-send" {
		t.Fatalf("the approval's audit line is wrong: %+v", approved[1])
	}
	for _, e := range approved {
		if len(e.Paths) == 0 || e.Paths[0] != doc {
			t.Fatalf("an audit line does not name the document: %+v", e)
		}
	}
}

// TestMCPResolveForSendHasNoAlternativeSpellingOfConsent guards the shape of
// the arguments themselves: a client cannot smuggle consent in under another
// name, or as a string, and a near-miss is rejected rather than ignored.
func TestMCPResolveForSendHasNoAlternativeSpellingOfConsent(t *testing.T) {
	vault := fixtureVault(t)
	doc := filepath.Join(filepath.Dir(vault), "Identity", "Alex-Rao", "Passport_Alex-Rao_Passport-Office_2024.txt")
	gate(t, doc)

	for _, args := range []string{
		`{"reference":` + quote(doc) + `,"yes":true}`,
		`{"reference":` + quote(doc) + `,"confrim":true}`,
		`{"reference":` + quote(doc) + `,"force":true}`,
		`{"reference":` + quote(doc) + `,"accept_proposal":true}`,
	} {
		msgs, _ := mcpSession(t, vault, callTool(1, "resolve_for_send", args))
		rpcErr, ok := msgs[0]["error"].(map[string]any)
		if !ok {
			env, _ := toolEnvelope(t, msgs[0])
			t.Fatalf("%s was accepted: %+v", args, env)
		}
		if rpcErr["code"] != float64(mcp.CodeInvalidParams) {
			t.Errorf("%s: code = %v, want invalid params", args, rpcErr["code"])
		}
	}

	// A string "true" is not a boolean and is not consent either.
	msgs, _ := mcpSession(t, vault, callTool(1, "resolve_for_send", `{"reference":`+quote(doc)+`,"confirm":"true"}`))
	if _, ok := msgs[0]["error"].(map[string]any); !ok {
		env, _ := toolEnvelope(t, msgs[0])
		t.Fatalf(`"confirm":"true" was accepted: %+v`, env)
	}
	if entries := auditEntries(t, vault); len(entries) != 0 {
		t.Fatalf("a rejected call reached the vault: %+v", entries)
	}
}

func TestMCPToolArgumentsCannotInjectFlags(t *testing.T) {
	vault := fixtureVault(t)
	// A query that looks like a flag must be a query, not a flag: --confirm
	// arriving through a free-text field would be exactly the bypass the gate
	// exists to prevent.
	msgs, _ := mcpSession(t, vault, callTool(1, "find", `{"query":"--confirm"}`))
	env, isErr := toolEnvelope(t, msgs[0])
	if isErr {
		t.Fatalf("a dash-leading query broke the call: %+v", env)
	}
	if env["count"] != float64(0) {
		t.Fatalf("a dash-leading query was not treated as text: %+v", env)
	}
}

func TestMCPServerSurvivesBadCallsAndKeepsStdoutClean(t *testing.T) {
	vault := fixtureVault(t)
	msgs, stderr := mcpSession(t, vault,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"ingest_propose","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"tag","arguments":{"paths":[]}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"find","arguments":{"tags":"invoice"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"find","arguments":{"person":"nobody at all"}}}`,
	)
	if len(msgs) != 4 {
		t.Fatalf("got %d responses, want 4: %+v", len(msgs), msgs)
	}
	for i := 0; i < 3; i++ {
		if _, ok := msgs[i]["error"].(map[string]any); !ok {
			t.Errorf("response %d should be a JSON-RPC error: %+v", i, msgs[i])
		}
	}
	// The server is still serving after three bad calls.
	env, isErr := toolEnvelope(t, msgs[3])
	if isErr || env["command"] != "find" {
		t.Fatalf("the server stopped working after bad input: %+v", msgs[3])
	}
	if strings.Contains(stderr, "{\"jsonrpc\"") {
		t.Errorf("a protocol message went to stderr: %s", stderr)
	}
}

// TestMCPMissingVaultIsAnErrorResult covers the case an agent hits first: the
// server started somewhere that is not a vault.
func TestMCPMissingVaultIsAnErrorResult(t *testing.T) {
	empty := t.TempDir()
	t.Setenv(VaultEnv, empty)
	msgs, _ := mcpSession(t, "", callTool(1, "find", `{}`))
	env, isErr := toolEnvelope(t, msgs[0])
	if !isErr {
		t.Fatalf("a missing vault was reported as success: %+v", env)
	}
	if env["status"] != StatusError {
		t.Fatalf("status = %v, want %s", env["status"], StatusError)
	}
	if msg, _ := env["message"].(string); !strings.Contains(msg, "vault.yaml") {
		t.Fatalf("the failure does not say what is missing: %+v", env)
	}
}

func TestMCPDescribeMatchesTheServedSurface(t *testing.T) {
	out, errw, code := run(t, "mcp", "--describe", "--json")
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, errw)
	}
	obj := decode(t, out)
	if obj["protocol_version"] != mcp.ProtocolVersion {
		t.Errorf("protocol_version = %v, want %v", obj["protocol_version"], mcp.ProtocolVersion)
	}
	described := obj["tools"].([]any)

	msgs, _ := mcpSession(t, "", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	served := msgs[0]["result"].(map[string]any)["tools"].([]any)
	if len(described) != len(served) {
		t.Fatalf("--describe lists %d tools, the server serves %d", len(described), len(served))
	}
	for i := range served {
		d := described[i].(map[string]any)
		s := served[i].(map[string]any)
		if d["name"] != s["name"] {
			t.Errorf("tool %d: --describe says %v, the server says %v", i, d["name"], s["name"])
		}
		if d["mutates"] != false {
			t.Errorf("%v is described as mutating; the surface is propose-only", d["name"])
		}
	}
}
