// Package mcp implements the Model Context Protocol over JSON-RPC 2.0 on a
// stdio stream, with nothing but the standard library (Global Constraint 11).
//
// # What is in here and what is not
//
// This package is protocol only. It knows about JSON-RPC framing, the three
// methods a stdio MCP server must answer (initialize, tools/list, tools/call)
// and how to report a failure without dying. It knows nothing about vaults,
// documents or tags: the tools are supplied by the caller as values, so the
// package cannot grow vault logic even by accident. internal/cli builds the
// Kagaz tool set on top of it.
//
// # Framing
//
// The stdio transport is newline-delimited JSON: exactly one JSON-RPC message
// per line, no length prefix, no embedded newlines (encoding/json never emits
// one inside a value). Anything a server wants to say that is not a protocol
// message goes to Server.Log — writing it to the stream would corrupt it.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// ProtocolVersion is the MCP revision this server implements and reports from
// initialize. MCP revisions are dated strings rather than semver.
const ProtocolVersion = "2025-06-18"

// SupportedVersions are the revisions this server will answer with when a
// client asks for one of them by name. A client that asks for anything else is
// answered with ProtocolVersion, which the specification allows: the client
// then decides whether it can live with it.
//
// The framing, the three methods and the shapes below are identical across
// these revisions; the differences (batching, structured tool output, elicitation)
// are features this server does not use.
var SupportedVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

// maxMessageBytes caps one incoming line. A stream that never delivers a
// newline must fail rather than grow a buffer until the process is killed.
const maxMessageBytes = 8 << 20

// JSON-RPC 2.0 error codes.
const (
	// CodeParseError means the line was not valid JSON.
	CodeParseError = -32700
	// CodeInvalidRequest means the JSON was not a valid JSON-RPC request.
	CodeInvalidRequest = -32600
	// CodeMethodNotFound means the method is not one this server answers.
	CodeMethodNotFound = -32601
	// CodeInvalidParams means the params were missing, mistyped or unknown.
	CodeInvalidParams = -32602
	// CodeInternalError means the server failed while handling a valid call.
	CodeInternalError = -32603
)

// Error is a JSON-RPC error object, and the error type a Handler returns when
// the call itself was malformed (as opposed to the tool's work failing, which
// is reported as a Result with IsError set).
type Error struct {
	// Code is one of the Code* constants.
	Code int `json:"code"`
	// Message is a one-line description.
	Message string `json:"message"`
	// Data carries optional detail.
	Data any `json:"data,omitempty"`
}

// Error implements error.
func (e *Error) Error() string { return e.Message }

// Errorf builds an Error with a formatted message.
func Errorf(code int, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// InvalidParams is the error a tool returns when its arguments are unusable.
func InvalidParams(err error) *Error {
	return &Error{Code: CodeInvalidParams, Message: err.Error()}
}

// Result is one tool call's outcome: the text handed back to the model, and
// whether it describes a failure.
//
// IsError is a property of the result, not of the JSON-RPC exchange: a tool
// that refuses to act (the confidential gate, a lint failure) has answered the
// call correctly and must not look like a broken server. The model sees the
// text either way, which is the point — the refusal is information.
type Result struct {
	// Text is the tool's output, verbatim.
	Text string
	// IsError marks the text as describing a failure or a refusal.
	IsError bool
}

// Handler runs one tool call. args is the raw `arguments` object from the
// request, or nil when the client sent none.
//
// Returning an error rejects the call at the protocol level: return an *Error
// to choose the code, or any other error for CodeInternalError. Returning a
// Result with IsError set instead reports that the tool ran and the answer was
// a failure — the usual case.
type Handler func(ctx context.Context, args json.RawMessage) (Result, error)

// Tool is one entry in tools/list and one dispatch target for tools/call.
type Tool struct {
	// Name is the identifier the client calls.
	Name string
	// Description is the agent's only documentation for this tool. It is
	// prose, and it is read by a model that has nothing else to go on.
	Description string
	// InputSchema is the JSON Schema of the arguments object. It must be a
	// JSON object; Server.Serve refuses to start otherwise.
	InputSchema json.RawMessage
	// Handler runs the call.
	Handler Handler
}

// Server is a stdio MCP server.
type Server struct {
	// Name and Version identify the server to the client.
	Name    string
	Version string
	// Tools is the complete, fixed tool surface.
	Tools []Tool
	// In is the request stream, one JSON-RPC message per line.
	In io.Reader
	// Out is the response stream. Nothing but protocol messages is ever
	// written to it.
	Out io.Writer
	// Log receives diagnostics. It is stderr for the real server, and may be
	// nil, which discards them.
	Log io.Writer

	mu sync.Mutex
}

// rpcRequest is an incoming JSON-RPC message. A message without an id is a
// notification and is never answered.
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// rpcResponse is an outgoing JSON-RPC message.
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// nullID is the id of a response to a message whose id could not be read.
var nullID = json.RawMessage("null")

// Serve reads messages until the stream ends or ctx is cancelled.
//
// It returns nil on a clean end of stream: a client that closes stdin has
// finished with the server, which is the normal way an MCP session ends. Every
// per-message failure is answered with a JSON-RPC error and the loop
// continues — a server that dies on a bad frame is unusable to an agent.
func (s *Server) Serve(ctx context.Context) error {
	if err := s.validate(); err != nil {
		return err
	}
	scanner := bufio.NewScanner(s.In)
	scanner.Buffer(make([]byte, 0, 64<<10), maxMessageBytes)

	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		if resp := s.handle(ctx, line); resp != nil {
			if err := s.write(resp); err != nil {
				return err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			// The rest of that line is still in the pipe, so the stream can no
			// longer be framed. Say so and stop, rather than parsing fragments.
			_ = s.write(&rpcResponse{JSONRPC: "2.0", ID: nullID,
				Error: Errorf(CodeParseError, "message exceeds %d bytes", maxMessageBytes)})
			return fmt.Errorf("mcp: %w", err)
		}
		return err
	}
	return nil
}

// validate rejects a server that could not answer tools/list honestly.
func (s *Server) validate() error {
	seen := map[string]bool{}
	for _, t := range s.Tools {
		if t.Name == "" || t.Handler == nil {
			return fmt.Errorf("mcp: tool %q needs a name and a handler", t.Name)
		}
		if seen[t.Name] {
			return fmt.Errorf("mcp: duplicate tool %q", t.Name)
		}
		seen[t.Name] = true
		if !json.Valid(t.InputSchema) || len(bytes.TrimSpace(t.InputSchema)) == 0 ||
			bytes.TrimSpace(t.InputSchema)[0] != '{' {
			return fmt.Errorf("mcp: tool %q has no JSON object input schema", t.Name)
		}
	}
	return nil
}

// write emits one message as a single line.
func (s *Server) write(resp *rpcResponse) error {
	data, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.Out.Write(append(data, '\n')); err != nil {
		return err
	}
	return nil
}

// logf writes a diagnostic to stderr, never to the protocol stream.
func (s *Server) logf(format string, args ...any) {
	if s.Log == nil {
		return
	}
	fmt.Fprintf(s.Log, format+"\n", args...)
}

// handle turns one line into the response to send, or nil for a notification.
func (s *Server) handle(ctx context.Context, line []byte) *rpcResponse {
	if line[0] == '[' {
		// JSON-RPC batching: valid JSON-RPC, but MCP's 2025-06-18 revision
		// removed it and this server never supported it. Saying so is more
		// useful than answering the first element and dropping the rest.
		return errorResponse(nullID, Errorf(CodeInvalidRequest, "JSON-RPC batches are not supported"))
	}
	var req rpcRequest
	dec := json.NewDecoder(bytes.NewReader(line))
	if err := dec.Decode(&req); err != nil {
		s.logf("mcp: parse error: %v", err)
		return errorResponse(nullID, Errorf(CodeParseError, "invalid JSON: %v", err))
	}

	id := req.ID
	if isNull(id) {
		id = nil
	}
	notification := id == nil

	var (
		result any
		rerr   *Error
	)
	switch {
	case req.JSONRPC != "2.0":
		rerr = Errorf(CodeInvalidRequest, "jsonrpc must be \"2.0\", got %q", req.JSONRPC)
	case req.Method == "":
		rerr = Errorf(CodeInvalidRequest, "method is required")
	default:
		result, rerr = s.dispatch(ctx, req.Method, req.Params, notification)
	}

	if notification {
		if rerr != nil {
			s.logf("mcp: notification %q: %s", req.Method, rerr.Message)
		}
		return nil
	}
	if rerr != nil {
		return errorResponse(id, rerr)
	}
	return &rpcResponse{JSONRPC: "2.0", ID: id, Result: result}
}

// dispatch answers one method.
func (s *Server) dispatch(ctx context.Context, method string, params json.RawMessage, notification bool) (any, *Error) {
	switch method {
	case "initialize":
		return s.initialize(params), nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": s.toolDescriptors()}, nil
	case "tools/call":
		return s.call(ctx, params)
	default:
		if notification {
			// Unknown notifications (notifications/initialized,
			// notifications/cancelled) are ignored by design: a notification
			// is never answered, so refusing one would only be noise.
			return nil, nil
		}
		return nil, Errorf(CodeMethodNotFound, "unknown method %q", method)
	}
}

// initializeParams is the subset of the initialize request this server reads.
type initializeParams struct {
	ProtocolVersion string `json:"protocolVersion"`
}

// initialize answers the handshake, echoing a protocol version the client
// named when it is one this server speaks.
func (s *Server) initialize(params json.RawMessage) any {
	version := ProtocolVersion
	var p initializeParams
	if len(params) > 0 && json.Unmarshal(params, &p) == nil && p.ProtocolVersion != "" {
		for _, v := range SupportedVersions {
			if v == p.ProtocolVersion {
				version = v
				break
			}
		}
	}
	return map[string]any{
		"protocolVersion": version,
		"capabilities": map[string]any{
			"tools": map[string]any{"listChanged": false},
		},
		"serverInfo": map[string]any{"name": s.Name, "version": s.Version},
	}
}

// toolDescriptor is one tools/list entry.
type toolDescriptor struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// toolDescriptors renders the tool surface in declaration order, which is the
// order a reader of the code sees.
func (s *Server) toolDescriptors() []toolDescriptor {
	out := make([]toolDescriptor, 0, len(s.Tools))
	for _, t := range s.Tools {
		out = append(out, toolDescriptor{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema})
	}
	return out
}

// callParams is a tools/call request.
type callParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// call runs one tool.
func (s *Server) call(ctx context.Context, params json.RawMessage) (any, *Error) {
	if len(params) == 0 {
		return nil, Errorf(CodeInvalidParams, "params are required for tools/call")
	}
	var p callParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, Errorf(CodeInvalidParams, "params: %v", err)
	}
	if p.Name == "" {
		return nil, Errorf(CodeInvalidParams, "params.name is required")
	}
	for i := range s.Tools {
		if s.Tools[i].Name != p.Name {
			continue
		}
		res, err := s.Tools[i].Handler(ctx, p.Arguments)
		if err != nil {
			var e *Error
			if errors.As(err, &e) {
				return nil, e
			}
			s.logf("mcp: tool %q failed: %v", p.Name, err)
			return nil, Errorf(CodeInternalError, "%v", err)
		}
		return toolResult(res), nil
	}
	return nil, Errorf(CodeInvalidParams, "unknown tool %q", p.Name)
}

// toolResult renders a Result as the MCP content block shape.
func toolResult(res Result) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": res.Text}},
		"isError": res.IsError,
	}
}

// errorResponse builds a failure reply.
func errorResponse(id json.RawMessage, err *Error) *rpcResponse {
	if id == nil {
		id = nullID
	}
	return &rpcResponse{JSONRPC: "2.0", ID: id, Error: err}
}

// isNull reports whether a raw id is absent or JSON null.
func isNull(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null"))
}

// DecodeArguments unmarshals a tool's arguments strictly: an unknown field is
// an error rather than a silently ignored one.
//
// Strictness is the safe direction here. A client that sends `{"confrim":true}`
// to resolve_for_send must be told, not quietly treated as not having
// confirmed — and a typo in a filter argument that silently widened a query is
// how the wrong document gets returned.
func DecodeArguments(raw json.RawMessage, dst any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("arguments: %w", err)
	}
	return nil
}
