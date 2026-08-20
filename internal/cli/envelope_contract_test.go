package cli

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// The envelope is decoded by scripts, by the MCP server, and by Kagaz for Mac,
// which ships from a separate repository on its own release cadence. Nothing
// updates all of them in one commit any more, so these tests exist to make a
// breaking change loud at the point it is made rather than silent until a user
// hits it. See docs/json-envelope-contract.md.

// TestEnvelopeSchemaVersionIsPinned pins the version as a literal on purpose.
//
// The other schema_version assertions in this package compare the emitted value
// against the SchemaVersion constant, which is tautological: bump the constant
// and they still pass. This one cannot be satisfied by bumping anything, which
// is the entire point.
func TestEnvelopeSchemaVersionIsPinned(t *testing.T) {
	const pinned = 1
	if SchemaVersion != pinned {
		t.Fatalf(`SchemaVersion is %d, pinned at %d.

Bumping it declares that an existing field changed meaning or disappeared, and
that every client built against v%d is now wrong. Adding a field is NOT a bump.

If the change really is breaking:
  1. read docs/json-envelope-contract.md ("Changing the envelope")
  2. update the pinned constant here
  3. refresh internal/cli/testdata/envelope/
  4. ship the client update before or with the CLI release`,
			SchemaVersion, pinned, pinned)
	}
}

// TestEnvelopeReservedKeys fixes the four keys the envelope owns. A payload may
// not use them, and a client may rely on them always meaning what they mean.
func TestEnvelopeReservedKeys(t *testing.T) {
	want := []string{"command", "schema_version", "status", "warnings"}
	got := make([]string, 0, len(reservedKeys))
	for k := range reservedKeys {
		got = append(got, k)
	}
	sort.Strings(got)
	if len(got) != len(want) {
		t.Fatalf("reserved keys = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("reserved keys = %v, want %v", got, want)
		}
	}
}

// TestEnvelopeGolden compares a rendered envelope against a committed fixture.
//
// The pinned-version test above catches a deliberate bump; this catches the
// accidental break — a field quietly renamed or dropped without anyone thinking
// of it as a contract change at all.
func TestEnvelopeGolden(t *testing.T) {
	type payload struct {
		Count   int      `json:"count"`
		Results []string `json:"results"`
	}
	got, err := Envelope(&Response{
		Command:  "find",
		Status:   StatusOK,
		Payload:  payload{Count: 2, Results: []string{"a", "b"}},
		Warnings: []string{"a warning"},
	})
	if err != nil {
		t.Fatalf("Envelope: %v", err)
	}

	golden := filepath.Join("testdata", "envelope", "find_ok.json")
	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Log("golden updated; re-run without UPDATE_GOLDEN")
		return
	}

	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden (regenerate with UPDATE_GOLDEN=1 go test ./internal/cli): %v", err)
	}
	if string(got) != string(want) {
		t.Errorf(`envelope has changed shape.

got:
%s
want:
%s
If this change is additive, regenerate: UPDATE_GOLDEN=1 go test ./internal/cli
If it renames or removes a field, it is breaking -- see
docs/json-envelope-contract.md before regenerating.`, got, want)
	}
}

// TestEnvelopeRejectsReservedPayloadKey guards the flattening rule: because the
// payload is merged into the envelope rather than nested, a payload field named
// like an envelope field would silently overwrite it.
func TestEnvelopeRejectsReservedPayloadKey(t *testing.T) {
	_, err := Envelope(&Response{
		Command: "find",
		Status:  StatusOK,
		Payload: map[string]any{"status": "sneaky"},
	})
	if err == nil {
		t.Fatal("a payload using the reserved key \"status\" was accepted; it must be rejected")
	}
}

// TestEnvelopeStatusesAreStable fixes the status vocabulary. Adding one is
// additive and fine -- update this list. Renaming or removing one is breaking.
func TestEnvelopeStatusesAreStable(t *testing.T) {
	for name, got := range map[string]string{
		"StatusOK":                   StatusOK,
		"StatusProposed":             StatusProposed,
		"StatusConfirmationRequired": StatusConfirmationRequired,
		"StatusFindings":             StatusFindings,
		"StatusError":                StatusError,
	} {
		want := map[string]string{
			"StatusOK":                   "ok",
			"StatusProposed":             "proposed",
			"StatusConfirmationRequired": "confirmation_required",
			"StatusFindings":             "findings",
			"StatusError":                "error",
		}[name]
		if got != want {
			t.Errorf("%s = %q, want %q -- renaming a status is a breaking change", name, got, want)
		}
	}
}

// TestExitCodesAreStable does the same for the exit codes, which commands.md
// documents as branchable.
func TestExitCodesAreStable(t *testing.T) {
	for _, tc := range []struct {
		name string
		got  int
		want int
	}{
		{"ExitOK", ExitOK, 0},
		{"ExitFailure", ExitFailure, 1},
		{"ExitUsage", ExitUsage, 2},
		{"ExitConfirmationRequired", ExitConfirmationRequired, 3},
		{"ExitFindings", ExitFindings, 4},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %d, want %d -- scripts branch on these", tc.name, tc.got, tc.want)
		}
	}
}
