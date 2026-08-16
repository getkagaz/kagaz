// Package cli implements the `kagaz` command surface on top of
// internal/vaultkit. It holds no vault logic of its own: every command in here
// composes vaultkit packages, formats their results, and gets out of the way.
//
// # One data path, two renderings
//
// Every command builds exactly one typed payload value and hands it to Emit.
// Emit either marshals that value into the JSON envelope or passes the very
// same value to the command's human renderer. There is deliberately no code
// path in which the human output and the `--json` output are computed
// separately, because two renderings computed twice are two renderings that
// eventually disagree.
package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
)

// SchemaVersion is the version of the `--json` envelope. It appears in every
// JSON response as `schema_version` and changes only when an existing field
// changes meaning or disappears; adding a field is not a version bump.
const SchemaVersion = 1

// Envelope statuses.
const (
	// StatusOK means the command did what it was asked to do.
	StatusOK = "ok"
	// StatusProposed means a mutating command stopped at the proposal, either
	// because of --propose-only or because confirmation was declined.
	StatusProposed = "proposed"
	// StatusConfirmationRequired means a gated operation needs explicit
	// consent (`resolve --for-send` without --confirm).
	StatusConfirmationRequired = "confirmation_required"
	// StatusFindings means a read-only check completed and found problems.
	StatusFindings = "findings"
	// StatusError means the command failed.
	StatusError = "error"
)

// Exit codes. They are part of the CLI contract: scripts branch on them.
const (
	// ExitOK is success.
	ExitOK = 0
	// ExitFailure is any runtime failure.
	ExitFailure = 1
	// ExitUsage is a bad invocation (unknown flag, missing argument).
	ExitUsage = 2
	// ExitConfirmationRequired is `resolve --for-send --json` without
	// --confirm, and any other refusal to act without consent.
	ExitConfirmationRequired = 3
	// ExitFindings is `kagaz lint` completing with error-severity findings.
	ExitFindings = 4
)

// Response is one command's complete result: the payload, how to render it for
// a human, and what the process should exit with.
type Response struct {
	// Command names the command in the JSON envelope, e.g. "find".
	Command string
	// Status is one of the Status* constants.
	Status string
	// Payload is the typed result. It must marshal to a JSON object; its
	// fields are flattened into the envelope so a documented shape such as
	// resolve's {"status","path","reason","message"} appears exactly as
	// documented rather than nested under a wrapper key.
	Payload any
	// Warnings are non-fatal problems. They appear in the envelope and on
	// stderr for a human.
	Warnings []string
	// Human renders Payload for a human. It receives the same value that is
	// marshalled into JSON, never a separately computed one.
	Human func(w io.Writer, payload any) error
	// Exit is the process exit code. Zero means success.
	Exit int
}

// reservedKeys are the envelope's own keys. A payload may not use them.
var reservedKeys = map[string]bool{
	"command": true, "status": true, "schema_version": true, "warnings": true,
}

// Envelope renders a response as the documented JSON object: the payload's own
// fields at the top level, plus `command`, `status` and `schema_version`.
func Envelope(res *Response) ([]byte, error) {
	obj := map[string]json.RawMessage{}
	if res.Payload != nil {
		raw, err := json.Marshal(res.Payload)
		if err != nil {
			return nil, fmt.Errorf("encode %s payload: %w", res.Command, err)
		}
		if len(raw) > 0 && raw[0] == '{' {
			if err := json.Unmarshal(raw, &obj); err != nil {
				return nil, fmt.Errorf("encode %s payload: %w", res.Command, err)
			}
			for k := range obj {
				if reservedKeys[k] {
					return nil, fmt.Errorf("%s payload uses reserved envelope key %q", res.Command, k)
				}
			}
		} else {
			obj["data"] = raw
		}
	}
	put := func(key string, v any) error {
		raw, err := json.Marshal(v)
		if err != nil {
			return err
		}
		obj[key] = raw
		return nil
	}
	if err := put("command", res.Command); err != nil {
		return nil, err
	}
	if err := put("status", res.Status); err != nil {
		return nil, err
	}
	if err := put("schema_version", SchemaVersion); err != nil {
		return nil, err
	}
	if len(res.Warnings) > 0 {
		if err := put("warnings", res.Warnings); err != nil {
			return nil, err
		}
	}
	return marshalStable(obj)
}

// marshalStable renders a flat object with sorted keys, so two runs over the
// same data produce byte-identical output.
func marshalStable(obj map[string]json.RawMessage) ([]byte, error) {
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := []byte("{\n")
	for i, k := range keys {
		key, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		var buf bytes.Buffer
		if err := json.Indent(&buf, obj[k], "  ", "  "); err != nil {
			return nil, err
		}
		out = append(out, "  "...)
		out = append(out, key...)
		out = append(out, ": "...)
		out = append(out, buf.Bytes()...)
		if i < len(keys)-1 {
			out = append(out, ',')
		}
		out = append(out, '\n')
	}
	out = append(out, "}\n"...)
	return out, nil
}

// errorPayload is the body of an error envelope.
type errorPayload struct {
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
}

// ErrorResponse builds the envelope emitted when a command fails.
func ErrorResponse(command string, err error, hint string, exit int) *Response {
	return &Response{
		Command: command,
		Status:  StatusError,
		Payload: errorPayload{Message: err.Error(), Hint: hint},
		Exit:    exit,
	}
}
