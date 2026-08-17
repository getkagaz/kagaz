package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/getkagaz/kagaz/internal/mcp"
	"github.com/spf13/cobra"
)

// MCPPayload is the `kagaz mcp --describe --json` body. The tool list is
// declared here because it is a contract, not an implementation detail: the
// surface stays propose-only, and nothing in it executes a mutation.
type MCPPayload struct {
	// Status is "ready" once the server is implemented.
	Status string `json:"server_status"`
	// Transport is the wire protocol the server speaks.
	Transport string `json:"transport"`
	// Protocol is the MCP revision reported by initialize.
	Protocol string `json:"protocol_version"`
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

// mcpCLIArgs is the argv a tool hands to the CLI, and the whole of what an MCP
// tool contributes to a call. Every tool is a thin wrapper by construction:
// it builds argv and returns what the CLI said, so an agent and a human read
// the same bytes from the same code path (Global Constraint 1).
type mcpCLIArgs []string

// mcpInvoke runs one CLI command in-process and returns its JSON envelope and
// exit code.
//
// It is the same entry point cmd/kagaz's main() calls, with --json always set
// and stdin empty, so no tool can ever be prompted at and none can inherit a
// terminal's consent. --vault is forwarded when the server was started with
// one, because the tools must look at the vault the operator chose.
func (r *Runtime) mcpInvoke(args mcpCLIArgs) (string, int) {
	argv := make([]string, 0, len(args)+2)
	if r.Vault != "" {
		argv = append(argv, "--vault", r.Vault)
	}
	argv = append(argv, args...)

	var out, errw bytes.Buffer
	code := Main(r.Version, argv, &out, &errw, strings.NewReader(""))

	// A failing command reports its envelope on stderr (reportError), a
	// succeeding one on stdout. Both are the documented envelope.
	body := strings.TrimSpace(out.String())
	if body == "" {
		body = strings.TrimSpace(errw.String())
	}
	if body == "" {
		body = fmt.Sprintf(`{"command":"kagaz","status":"error","schema_version":%d,"message":"the command produced no output","exit":%d}`,
			SchemaVersion, code)
	}
	return body, code
}

// mcpResult turns a CLI invocation into a tool result. A non-zero exit is
// reported as an error result carrying the envelope verbatim: a refusal
// (confirmation_required, exit 3) is information the agent must read, not a
// broken call.
func (r *Runtime) mcpResult(args mcpCLIArgs) (mcp.Result, error) {
	body, code := r.mcpInvoke(args)
	return mcp.Result{Text: body, IsError: code != ExitOK}, nil
}

// mcpFlags appends `--name value` for each non-empty value.
func mcpFlags(argv mcpCLIArgs, name string, values ...string) mcpCLIArgs {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			argv = append(argv, name, v)
		}
	}
	return argv
}

// mcpPositional appends the `--` separator and the positional arguments.
//
// The separator is not decoration: it stops a document path or a query string
// that begins with a dash from being read as a flag. An MCP client's arguments
// are attacker-adjacent input, and this is the one place they meet a command
// line.
func mcpPositional(argv mcpCLIArgs, args ...string) mcpCLIArgs {
	kept := make([]string, 0, len(args))
	for _, a := range args {
		if a != "" {
			kept = append(kept, a)
		}
	}
	if len(kept) == 0 {
		return argv
	}
	return append(append(argv, "--"), kept...)
}

// mcpQueryArgs are the filters shared by find and resolve_for_send. They are
// exactly `kagaz find`'s flags, under exactly their flag names.
type mcpQueryArgs struct {
	Person  string   `json:"person,omitempty"`
	Company string   `json:"company,omitempty"`
	Area    string   `json:"area,omitempty"`
	DocType string   `json:"doctype,omitempty"`
	Tags    []string `json:"tags,omitempty"`
	Active  bool     `json:"active,omitempty"`
	Period  string   `json:"period,omitempty"`
}

// apply appends the filter flags to argv.
func (q mcpQueryArgs) apply(argv mcpCLIArgs) mcpCLIArgs {
	argv = mcpFlags(argv, "--person", q.Person)
	argv = mcpFlags(argv, "--company", q.Company)
	argv = mcpFlags(argv, "--area", q.Area)
	argv = mcpFlags(argv, "--doctype", q.DocType)
	argv = mcpFlags(argv, "--tag", q.Tags...)
	if q.Active {
		argv = append(argv, "--active")
	}
	return mcpFlags(argv, "--period", q.Period)
}

// mcpFindArgs is the find tool's argument object.
type mcpFindArgs struct {
	mcpQueryArgs
	Query string `json:"query,omitempty"`
	Limit int    `json:"limit,omitempty"`
}

// mcpIngestArgs is the ingest_propose tool's argument object.
type mcpIngestArgs struct {
	Paths []string `json:"paths"`
}

// mcpTagArgs is the tag tool's argument object.
type mcpTagArgs struct {
	Paths  []string `json:"paths"`
	Add    []string `json:"add,omitempty"`
	Remove []string `json:"remove,omitempty"`
	Force  bool     `json:"force,omitempty"`
}

// mcpResolveArgs is the resolve_for_send tool's argument object.
type mcpResolveArgs struct {
	mcpQueryArgs
	Reference string `json:"reference,omitempty"`
	// Confirm is the explicit consent `kagaz resolve --for-send --confirm`
	// requires. The server never supplies it: an absent or false Confirm is a
	// refusal, and there is no other argument, default or code path that
	// approves a gated resolution (safety invariant 7).
	Confirm bool `json:"confirm,omitempty"`
}

// mcpToolSet builds the four-tool surface. It is fixed: an agent proposes and
// a human executes, so no tool here applies a proposal.
func mcpToolSet(rt *Runtime) []mcp.Tool {
	return []mcp.Tool{
		{
			Name: "find",
			Description: "Search the Kagaz vault. Read-only. Filters combine with AND; `query` is " +
				"free text matched against filename, path, sidecar text and extracted fields. " +
				"Returns the `kagaz find --json` envelope: {vault_root, query, count, truncated, " +
				"results[{path, rel_path, name, category, doctype, owners, identifier, year, " +
				"modifier, tags, parsed, has_sidecar, evicted, size, mod_time}]}. `evicted: true` " +
				"means the file is an iCloud placeholder whose bytes are not on this machine; use " +
				"resolve_for_send to bring one down.",
			InputSchema: mcpFindSchema,
			Handler: func(_ context.Context, raw json.RawMessage) (mcp.Result, error) {
				var a mcpFindArgs
				if err := mcp.DecodeArguments(raw, &a); err != nil {
					return mcp.Result{}, mcp.InvalidParams(err)
				}
				argv := a.apply(mcpCLIArgs{"find", "--json"})
				if a.Limit > 0 {
					argv = append(argv, "--limit", fmt.Sprint(a.Limit))
				}
				return rt.mcpResult(mcpPositional(argv, a.Query))
			},
		},
		{
			Name: "ingest_propose",
			Description: "Analyse loose files (OCR, classification, field extraction) and return " +
				"where Kagaz would file each one. Proposes only: nothing is moved, copied or " +
				"written. Returns the `kagaz ingest --propose-only --json` envelope: " +
				"{proposals[{source, dest, doctype, category, owners, identifier, year, " +
				"confidence, skip, skip_reason, warnings, ...}], executed: false, guidance[]}. " +
				"There is deliberately no tool that executes a proposal — a human runs " +
				"`kagaz ingest <path> --yes` after reading it.",
			InputSchema: mcpIngestSchema,
			Handler: func(_ context.Context, raw json.RawMessage) (mcp.Result, error) {
				var a mcpIngestArgs
				if err := mcp.DecodeArguments(raw, &a); err != nil {
					return mcp.Result{}, mcp.InvalidParams(err)
				}
				if len(a.Paths) == 0 {
					return mcp.Result{}, mcp.Errorf(mcp.CodeInvalidParams, "paths must name at least one file or directory")
				}
				return rt.mcpResult(mcpPositional(mcpCLIArgs{"ingest", "--json", "--propose-only"}, a.Paths...))
			},
		},
		{
			Name: "tag",
			Description: "Show the Finder-tag change that adding and/or removing the given tags " +
				"would make to each document, validated against the vault's controlled " +
				"vocabulary. Proposes only: no tag is written. Returns the " +
				"`kagaz tag --propose-only --json` envelope: {changes[{path, before, after, " +
				"added, removed}], forced, unvalidated[]}. A human applies it with " +
				"`kagaz tag <path> --add <tag> --yes`.",
			InputSchema: mcpTagSchema,
			Handler: func(_ context.Context, raw json.RawMessage) (mcp.Result, error) {
				var a mcpTagArgs
				if err := mcp.DecodeArguments(raw, &a); err != nil {
					return mcp.Result{}, mcp.InvalidParams(err)
				}
				if len(a.Paths) == 0 {
					return mcp.Result{}, mcp.Errorf(mcp.CodeInvalidParams, "paths must name at least one document")
				}
				argv := mcpCLIArgs{"tag", "--json", "--propose-only"}
				argv = mcpFlags(argv, "--add", a.Add...)
				argv = mcpFlags(argv, "--remove", a.Remove...)
				if a.Force {
					argv = append(argv, "--force")
				}
				return rt.mcpResult(mcpPositional(argv, a.Paths...))
			},
		},
		{
			Name: "resolve_for_send",
			Description: "Resolve one document to an absolute path on this machine for handoff " +
				"OUTSIDE the vault (attaching it to an email, uploading it). " +
				"REQUIRES CONFIRMATION AND IS ALWAYS AUDITED. Called without `confirm: true` on " +
				"a gated document it returns status \"confirmation_required\" and NO path; the " +
				"audit line is written either way, before any path is handed over. `confirm: " +
				"true` is the human-authorised consent — do not set it on your own initiative; " +
				"ask first, then call again. Name the document with `reference` (a path) or with " +
				"the same filters find takes; a reference matching more than one document is an " +
				"error rather than a guess. Returns the `kagaz resolve --for-send --json` " +
				"envelope: {path, resolved_path, doctype, tags, for_send, gated, confirmed, " +
				"materialized, reason, message}. `resolved_path` appears only when the bytes are " +
				"really on this machine: an iCloud placeholder that cannot be downloaded is a " +
				"failure, never a path.",
			InputSchema: mcpResolveSchema,
			Handler: func(_ context.Context, raw json.RawMessage) (mcp.Result, error) {
				var a mcpResolveArgs
				if err := mcp.DecodeArguments(raw, &a); err != nil {
					return mcp.Result{}, mcp.InvalidParams(err)
				}
				argv := a.apply(mcpCLIArgs{"resolve", "--json", "--for-send"})
				// The only place --confirm can be added, and only from the
				// caller's explicit argument.
				if a.Confirm {
					argv = append(argv, "--confirm")
				}
				return rt.mcpResult(mcpPositional(argv, a.Reference))
			},
		},
	}
}

// The input schemas below are the agent's only documentation for the arguments.
// They are written as literal JSON rather than built from Go maps so that what
// a client receives is what a reader of this file sees.
var (
	mcpQuerySchemaFields = `
    "person":  {"type": "string", "description": "person display name or tag, e.g. \"Alex Rao\" or \"alex-rao\""},
    "company": {"type": "string", "description": "company tag from the vault vocabulary"},
    "area":    {"type": "string", "description": "area tag from the vault vocabulary"},
    "doctype": {"type": "string", "description": "doctype name from the vault catalog, e.g. \"invoice\""},
    "tags":    {"type": "array", "items": {"type": "string"}, "description": "Finder tags that must all be present"},
    "active":  {"type": "boolean", "description": "only documents tagged active"},
    "period":  {"type": "string", "description": "calendar or fiscal period: 2026, FY2026 or FY2026Q3"}`

	mcpFindSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "query":   {"type": "string", "description": "free-text query matched against filename, path, sidecar text and extracted fields"},
    "limit":   {"type": "integer", "minimum": 0, "description": "stop after this many results; 0 means no limit"},` +
		mcpQuerySchemaFields + `
  },
  "additionalProperties": false
}`)

	mcpIngestSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "paths": {
      "type": "array",
      "items": {"type": "string"},
      "minItems": 1,
      "description": "files or directories to analyse; they are read, never moved or modified"
    }
  },
  "required": ["paths"],
  "additionalProperties": false
}`)

	mcpTagSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "paths":  {"type": "array", "items": {"type": "string"}, "minItems": 1, "description": "documents to describe the tag change for; absolute, or relative to the vault root"},
    "add":    {"type": "array", "items": {"type": "string"}, "description": "tags that would be added"},
    "remove": {"type": "array", "items": {"type": "string"}, "description": "tags that would be removed"},
    "force":  {"type": "boolean", "description": "allow tags outside the controlled vocabulary; each one becomes a kagaz lint finding"}
  },
  "required": ["paths"],
  "additionalProperties": false
}`)

	mcpResolveSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "reference": {"type": "string", "description": "path of the document to resolve; omit to select it with the filters below"},
    "confirm":   {"type": "boolean", "default": false, "description": "explicit, human-authorised consent to resolve a gated document for external send. Without it a gated document returns status \"confirmation_required\" and no path. Every call is audited, confirmed or refused."},` +
		mcpQuerySchemaFields + `
  },
  "additionalProperties": false
}`)
)

// mcpToolSummary renders the surface for `kagaz mcp --describe`.
func mcpToolSummary(rt *Runtime) []MCPTool {
	tools := mcpToolSet(rt)
	out := make([]MCPTool, 0, len(tools))
	for _, t := range tools {
		out = append(out, MCPTool{Name: t.Name, Mutates: false, Description: firstSentence(t.Description)})
	}
	return out
}

// firstSentence trims a tool description down to its opening sentence, which
// is what a one-line summary has room for.
func firstSentence(s string) string {
	if i := strings.Index(s, ". "); i > 0 {
		return s[:i+1]
	}
	return s
}

func newMCPCommand(rt *Runtime) *cobra.Command {
	var describe bool

	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Start the stdio MCP server",
		Long: "mcp starts the Model Context Protocol server on stdin/stdout: newline-delimited\n" +
			"JSON-RPC 2.0, one message per line, answering initialize, tools/list and\n" +
			"tools/call. It runs until stdin closes.\n\n" +
			"The surface is propose-only: an agent can query, propose and ask for a\n" +
			"resolution, and a human executes. There is no execute tool, and\n" +
			"resolve_for_send cannot auto-confirm the confidential gate.\n\n" +
			"Every tool is the corresponding `kagaz ... --json` command, run through the\n" +
			"very same entry point the binary uses, so an agent and a human see the same\n" +
			"bytes. --describe prints the surface instead of serving it.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if describe {
				return rt.Emit(&Response{
					Command: "mcp",
					Status:  StatusOK,
					Payload: MCPPayload{
						Status:    "ready",
						Transport: "stdio JSON-RPC 2.0 (newline-delimited)",
						Protocol:  mcp.ProtocolVersion,
						Tools:     mcpToolSummary(rt),
						Message:   "run `kagaz mcp` to serve this surface on stdin/stdout",
					},
					Human: humanMCP,
				})
			}
			server := &mcp.Server{
				Name:    "kagaz",
				Version: rt.Version,
				Tools:   mcpToolSet(rt),
				In:      rt.In,
				// rt.Out is stdout, and while this command is serving, the
				// JSON-RPC stream is the ONLY thing that may be written there
				// -- one stray human-readable line corrupts the protocol. That
				// is why nothing else in this branch prints, and why every
				// diagnostic below goes to rt.Err.
				Out: rt.Out,
				Log: rt.Err,
			}
			return server.Serve(cmd.Context())
		},
	}
	cmd.Flags().BoolVar(&describe, "describe", false, "print the tool surface and exit instead of serving")
	return cmd
}

func humanMCP(w io.Writer, payload any) error {
	p, ok := payload.(MCPPayload)
	if !ok {
		return fmt.Errorf("mcp: unexpected payload %T", payload)
	}
	fmt.Fprintf(w, "%s\n\nTransport: %s, protocol %s\nTools (%d), all propose-only:\n",
		p.Message, p.Transport, p.Protocol, len(p.Tools))
	for _, t := range p.Tools {
		fmt.Fprintf(w, "  %-18s %s\n", t.Name, t.Description)
	}
	return nil
}
