package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// echoServer builds a server with one trivial tool, writing to buffers.
func echoServer(in string) (*Server, *bytes.Buffer, *bytes.Buffer) {
	var out, logs bytes.Buffer
	s := &Server{
		Name: "test", Version: "1.2.3",
		In: strings.NewReader(in), Out: &out, Log: &logs,
		Tools: []Tool{
			{
				Name:        "echo",
				Description: "Echo the text argument back.",
				InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"},"fail":{"type":"boolean"}},"additionalProperties":false}`),
				Handler: func(_ context.Context, raw json.RawMessage) (Result, error) {
					var a struct {
						Text string `json:"text"`
						Fail bool   `json:"fail"`
					}
					if err := DecodeArguments(raw, &a); err != nil {
						return Result{}, InvalidParams(err)
					}
					return Result{Text: a.Text, IsError: a.Fail}, nil
				},
			},
		},
	}
	return s, &out, &logs
}

// serve runs the server over in and returns the response lines, parsed.
func serve(t *testing.T, in string) ([]map[string]any, string) {
	t.Helper()
	s, out, logs := echoServer(in)
	if err := s.Serve(context.Background()); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	var msgs []map[string]any
	for _, line := range strings.Split(strings.TrimRight(out.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("response line is not JSON: %v\n%s", err, line)
		}
		msgs = append(msgs, m)
	}
	return msgs, logs.String()
}

func TestFramingIsOneJSONMessagePerLine(t *testing.T) {
	in := `{"jsonrpc":"2.0","id":1,"method":"ping"}
{"jsonrpc":"2.0","id":2,"method":"ping"}

{"jsonrpc":"2.0","id":"three","method":"ping"}
`
	msgs, _ := serve(t, in)
	if len(msgs) != 3 {
		t.Fatalf("got %d responses, want 3: %+v", len(msgs), msgs)
	}
	for i, want := range []any{float64(1), float64(2), "three"} {
		if msgs[i]["jsonrpc"] != "2.0" {
			t.Errorf("response %d: jsonrpc = %v", i, msgs[i]["jsonrpc"])
		}
		if msgs[i]["id"] != want {
			t.Errorf("response %d: id = %v, want %v", i, msgs[i]["id"], want)
		}
	}
}

func TestNotificationIsNeverAnswered(t *testing.T) {
	msgs, _ := serve(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}
{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}
{"jsonrpc":"2.0","id":null,"method":"ping"}
`)
	if len(msgs) != 0 {
		t.Fatalf("a notification was answered: %+v", msgs)
	}
}

func TestInitializeReportsProtocolAndTools(t *testing.T) {
	msgs, _ := serve(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"c","version":"0"}}}`)
	res := msgs[0]["result"].(map[string]any)
	if res["protocolVersion"] != ProtocolVersion {
		t.Errorf("protocolVersion = %v, want %v", res["protocolVersion"], ProtocolVersion)
	}
	caps := res["capabilities"].(map[string]any)
	if _, ok := caps["tools"]; !ok {
		t.Errorf("no tools capability: %+v", caps)
	}
	info := res["serverInfo"].(map[string]any)
	if info["name"] != "test" || info["version"] != "1.2.3" {
		t.Errorf("serverInfo = %+v", info)
	}
}

func TestInitializeNegotiatesAKnownVersionAndFallsBack(t *testing.T) {
	for _, tc := range []struct{ asked, want string }{
		{"2024-11-05", "2024-11-05"},
		{"2099-01-01", ProtocolVersion},
		{"", ProtocolVersion},
	} {
		msgs, _ := serve(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"`+tc.asked+`"}}`)
		got := msgs[0]["result"].(map[string]any)["protocolVersion"]
		if got != tc.want {
			t.Errorf("asked %q: got %v, want %v", tc.asked, got, tc.want)
		}
	}
}

func TestToolsListCarriesNameDescriptionAndObjectSchema(t *testing.T) {
	msgs, _ := serve(t, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	tools := msgs[0]["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("got %d tools", len(tools))
	}
	tl := tools[0].(map[string]any)
	if tl["name"] != "echo" || tl["description"] == "" {
		t.Errorf("tool entry = %+v", tl)
	}
	schema := tl["inputSchema"].(map[string]any)
	if schema["type"] != "object" {
		t.Errorf("inputSchema is not an object schema: %+v", schema)
	}
}

func TestToolsCallReturnsContentAndIsError(t *testing.T) {
	msgs, _ := serve(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"text":"hi"}}}
{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{"text":"no","fail":true}}}`)
	for i, want := range []struct {
		text    string
		isError bool
	}{{"hi", false}, {"no", true}} {
		res := msgs[i]["result"].(map[string]any)
		content := res["content"].([]any)[0].(map[string]any)
		if content["type"] != "text" || content["text"] != want.text {
			t.Errorf("%d: content = %+v", i, content)
		}
		if res["isError"] != want.isError {
			t.Errorf("%d: isError = %v, want %v", i, res["isError"], want.isError)
		}
	}
}

func TestMalformedInputIsAStructuredErrorAndTheServerSurvives(t *testing.T) {
	cases := []struct {
		name string
		line string
		code float64
	}{
		{"not json", `{oops`, CodeParseError},
		{"not an object", `"just a string"`, CodeParseError},
		{"batch", `[{"jsonrpc":"2.0","id":1,"method":"ping"}]`, CodeInvalidRequest},
		{"wrong version", `{"jsonrpc":"1.0","id":1,"method":"ping"}`, CodeInvalidRequest},
		{"no method", `{"jsonrpc":"2.0","id":1}`, CodeInvalidRequest},
		{"unknown method", `{"jsonrpc":"2.0","id":1,"method":"vault/delete"}`, CodeMethodNotFound},
		{"call without params", `{"jsonrpc":"2.0","id":1,"method":"tools/call"}`, CodeInvalidParams},
		{"call without name", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"arguments":{}}}`, CodeInvalidParams},
		{"unknown tool", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"rm","arguments":{}}}`, CodeInvalidParams},
		{"bad argument type", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"text":42}}}`, CodeInvalidParams},
		{"unknown argument", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"txt":"hi"}}}`, CodeInvalidParams},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Every bad line is followed by a good one: the server must still
			// be answering afterwards.
			msgs, _ := serve(t, tc.line+"\n"+`{"jsonrpc":"2.0","id":99,"method":"ping"}`)
			if len(msgs) != 2 {
				t.Fatalf("got %d responses, want the error and the ping: %+v", len(msgs), msgs)
			}
			rpcErr, ok := msgs[0]["error"].(map[string]any)
			if !ok {
				t.Fatalf("no error object: %+v", msgs[0])
			}
			if rpcErr["code"] != tc.code {
				t.Errorf("code = %v, want %v (%v)", rpcErr["code"], tc.code, rpcErr["message"])
			}
			if msgs[0]["result"] != nil {
				t.Errorf("an error response carried a result: %+v", msgs[0])
			}
			if msgs[1]["id"] != float64(99) {
				t.Errorf("the server stopped answering after a bad frame: %+v", msgs[1])
			}
		})
	}
}

func TestDiagnosticsNeverReachTheProtocolStream(t *testing.T) {
	msgs, logs := serve(t, "{not json\n"+`{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	if len(msgs) != 2 {
		t.Fatalf("got %d responses: %+v", len(msgs), msgs)
	}
	if !strings.Contains(logs, "parse error") {
		t.Errorf("the parse error was not logged to the diagnostic stream: %q", logs)
	}
}

func TestServeRefusesAnInvalidToolSurface(t *testing.T) {
	cases := map[string][]Tool{
		"no handler":   {{Name: "x", InputSchema: json.RawMessage(`{}`)}},
		"no name":      {{Handler: func(context.Context, json.RawMessage) (Result, error) { return Result{}, nil }, InputSchema: json.RawMessage(`{}`)}},
		"no schema":    {{Name: "x", Handler: func(context.Context, json.RawMessage) (Result, error) { return Result{}, nil }}},
		"array schema": {{Name: "x", InputSchema: json.RawMessage(`[]`), Handler: func(context.Context, json.RawMessage) (Result, error) { return Result{}, nil }}},
		"duplicate": {
			{Name: "x", InputSchema: json.RawMessage(`{}`), Handler: func(context.Context, json.RawMessage) (Result, error) { return Result{}, nil }},
			{Name: "x", InputSchema: json.RawMessage(`{}`), Handler: func(context.Context, json.RawMessage) (Result, error) { return Result{}, nil }},
		},
	}
	for name, tools := range cases {
		s := &Server{Name: "t", Version: "0", Tools: tools, In: strings.NewReader(""), Out: &bytes.Buffer{}}
		if err := s.Serve(context.Background()); err == nil {
			t.Errorf("%s: Serve accepted an invalid tool surface", name)
		}
	}
}

func TestServeStopsAtEndOfStream(t *testing.T) {
	s, out, _ := echoServer("")
	if err := s.Serve(context.Background()); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("an empty session wrote to the stream: %q", out.String())
	}
}

func TestServeStopsOnAnOversizedMessage(t *testing.T) {
	s, out, _ := echoServer(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"text":"` +
		strings.Repeat("a", maxMessageBytes+1) + `"}}}`)
	if err := s.Serve(context.Background()); err == nil {
		t.Fatal("an unframeable stream was accepted")
	}
	if !strings.Contains(out.String(), "-32700") {
		t.Errorf("no parse error reported to the client: %q", out.String())
	}
}
