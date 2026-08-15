package classify

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/getkagaz/kagaz/internal/vaultkit/config"
	"github.com/getkagaz/kagaz/internal/vaultkit/doctypes"
)

// invoiceText matches the built-in "invoice" doctype on several keywords, so
// the rules tier is confident about it.
const invoiceText = `TAX INVOICE
Invoice Number: INV-2024-0912
Invoice No: INV-2024-0912
Bill To: Acme Corporation
Total: 4800.00
Date: 12/03/2024
`

// noiseText matches nothing in the catalog.
const noiseText = `Notes from a walk. The weather was fine and the dog was pleased.
Nothing here resembles any document the catalog knows about.
`

// testCatalog resolves the built-in catalog against a default vault config.
func testCatalog(t *testing.T) *doctypes.Catalog {
	t.Helper()
	cfg, err := config.Parse([]byte("version: 1\n"))
	if err != nil {
		t.Fatalf("config.Parse: %v", err)
	}
	cat, err := doctypes.Resolve(cfg)
	if err != nil {
		t.Fatalf("doctypes.Resolve: %v", err)
	}
	return cat
}

// fixture reads a recorded helper response. Tests never invoke a real helper:
// that is what keeps them running on Linux CI.
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	return data
}

// stubRunner returns a helperRunner that replays out/err regardless of input.
func stubRunner(out []byte, err error) helperRunner {
	return func(context.Context, string, []string, string) ([]byte, error) {
		return out, err
	}
}

// found is a discovery seam that always reports an installed helper.
func found() (string, bool) { return "/fake/kagaz-machelper", true }

// missing is a discovery seam that always reports nothing installed.
func missing() (string, bool) { return "", false }

func TestValidate(t *testing.T) {
	cat := testCatalog(t)

	tests := []struct {
		name     string
		in       Result
		min      float64
		wantOK   bool
		wantType string
		wantCat  string
	}{
		{
			name:     "known doctype above threshold",
			in:       Result{DocType: "invoice", Category: "financial", Confidence: 0.92},
			min:      0.5,
			wantOK:   true,
			wantType: "invoice",
			wantCat:  "financial",
		},
		{
			name:     "catalog overrides an invented category",
			in:       Result{DocType: "invoice", Category: "mythical", Confidence: 0.92},
			min:      0.5,
			wantOK:   true,
			wantType: "invoice",
			wantCat:  "financial",
		},
		{
			name:   "doctype outside the catalog",
			in:     Result{DocType: "unicorn-permit", Category: "mythical", Confidence: 0.99},
			min:    0.5,
			wantOK: false,
		},
		{
			name:   "below threshold",
			in:     Result{DocType: "invoice", Category: "financial", Confidence: 0.31},
			min:    0.5,
			wantOK: false,
		},
		{
			name:   "unclassified is never accepted as a doctype",
			in:     Result{DocType: doctypes.Unclassified, Confidence: 1},
			min:    0.5,
			wantOK: false,
		},
		{
			name:   "empty doctype",
			in:     Result{DocType: "  ", Confidence: 1},
			min:    0.5,
			wantOK: false,
		},
		{
			name:     "doctype is slugged before lookup",
			in:       Result{DocType: "Tax Return", Confidence: 0.8},
			min:      0.5,
			wantOK:   true,
			wantType: "tax-return",
			wantCat:  "financial",
		},
		{
			name:     "confidence above 1 is clamped",
			in:       Result{DocType: "invoice", Confidence: 4.2},
			min:      0.5,
			wantOK:   true,
			wantType: "invoice",
			wantCat:  "financial",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := validate(cat, tc.in, tc.min)
			if ok != tc.wantOK {
				t.Fatalf("validate ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if got.DocType != tc.wantType {
				t.Errorf("DocType = %q, want %q", got.DocType, tc.wantType)
			}
			if got.Category != tc.wantCat {
				t.Errorf("Category = %q, want %q", got.Category, tc.wantCat)
			}
			if got.Confidence < 0 || got.Confidence > 1 {
				t.Errorf("Confidence = %v, want 0-1", got.Confidence)
			}
		})
	}
}

func TestValidateRejectsNaN(t *testing.T) {
	cat := testCatalog(t)
	nan := 0.0
	nan = nan / nan // NaN without importing math into the test
	if _, ok := validate(cat, Result{DocType: "invoice", Confidence: nan}, 0.5); ok {
		t.Fatal("validate accepted a NaN confidence")
	}
}

func TestModelBasename(t *testing.T) {
	tests := map[string]string{
		"mlx-community/Qwen2.5-3B-Instruct-4bit": "Qwen2.5-3B-Instruct-4bit",
		"Qwen2.5-3B-Instruct-4bit":               "Qwen2.5-3B-Instruct-4bit",
		"a/b/c":                                  "c",
		"":                                       "",
	}
	for in, want := range tests {
		if got := modelBasename(in); got != want {
			t.Errorf("modelBasename(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMergeFieldsPrefersDeterministic(t *testing.T) {
	got := mergeFields(
		map[string]string{"amount": "4800.00"},
		map[string]string{"amount": "about 4800", "vendor": "Acme"},
	)
	if got["amount"] != "4800.00" {
		t.Errorf("amount = %q, want the deterministic value", got["amount"])
	}
	if got["vendor"] != "Acme" {
		t.Errorf("vendor = %q, want the model value to fill the gap", got["vendor"])
	}
	if mergeFields(nil, nil) != nil {
		t.Error("mergeFields(nil, nil) should be nil, not an empty map")
	}
	if mergeFields(nil, map[string]string{" ": "x", "k": " "}) != nil {
		t.Error("blank keys and values should be dropped, leaving nil")
	}
}

func TestRequestTextIsClipped(t *testing.T) {
	long := make([]byte, maxText+500)
	for i := range long {
		long[i] = 'a'
	}
	req := Request{Text: string(long)}
	if len(req.text()) != maxText {
		t.Fatalf("clipped length = %d, want %d", len(req.text()), maxText)
	}
	// A multi-byte rune straddling the boundary must not be cut in half.
	runes := make([]byte, 0, maxText+8)
	for len(runes) < maxText+4 {
		runes = append(runes, "é"...)
	}
	clipped := Request{Text: string(runes)}.text()
	for _, r := range clipped {
		if r == '�' {
			t.Fatal("clipping produced an invalid rune")
		}
	}
}

func TestRulesBackend(t *testing.T) {
	cat := testCatalog(t)
	r := &Rules{Catalog: cat}

	if r.Name() != config.EngineRules {
		t.Errorf("Name() = %q, want %q", r.Name(), config.EngineRules)
	}
	if !r.Available() {
		t.Error("rules must always be available")
	}

	res, err := r.Classify(context.Background(), Request{Text: invoiceText})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if res.DocType != "invoice" {
		t.Fatalf("DocType = %q, want invoice", res.DocType)
	}
	if res.Engine != config.EngineRules {
		t.Errorf("Engine = %q, want rules", res.Engine)
	}
	if res.Confidence <= 0 || res.Confidence > 0.95 {
		t.Errorf("Confidence = %v, want (0, 0.95]", res.Confidence)
	}
	if res.Fields["invoice_number"] != "INV-2024-0912" {
		t.Errorf("invoice_number = %q, want INV-2024-0912", res.Fields["invoice_number"])
	}

	noise, err := r.Classify(context.Background(), Request{Text: noiseText})
	if err != nil {
		t.Fatalf("Classify(noise): %v", err)
	}
	if noise.DocType != doctypes.Unclassified || noise.Confidence != 0 {
		t.Errorf("noise = %+v, want unclassified with zero confidence", noise)
	}

	if _, err := (&Rules{}).Classify(context.Background(), Request{Text: invoiceText}); err == nil {
		t.Error("rules without a catalog should report an error")
	}
}

func TestDecodeClassifyResponse(t *testing.T) {
	tests := []struct {
		name    string
		file    string
		wantErr string
	}{
		{name: "good", file: "classify_invoice.json"},
		{name: "structured error", file: "classify_error.json", wantErr: "Foundation Models are not enabled"},
		{name: "unknown contract", file: "classify_bad_contract.json", wantErr: "unsupported contract version 2"},
		{name: "malformed json", file: "classify_malformed.json", wantErr: "decoding response"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := decodeClassifyResponse("kagaz-machelper", "apple", fixture(t, tc.file))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if res.DocType != "invoice" || res.Confidence != 0.92 {
					t.Fatalf("res = %+v, want invoice/0.92", res)
				}
				if res.Engine != "apple" {
					t.Errorf("Engine = %q, want apple", res.Engine)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected an error containing %q, got result %+v", tc.wantErr, res)
			}
			if !contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}

	t.Run("empty doctype", func(t *testing.T) {
		_, err := decodeClassifyResponse("kagaz-machelper", "apple", []byte(`{"contract":1,"doctype":"  "}`))
		if err == nil || !contains(err.Error(), "no doctype") {
			t.Fatalf("error = %v, want a no-doctype error", err)
		}
	})
}

func TestDecodeProbeResponse(t *testing.T) {
	if ok, reason := decodeProbeResponse(fixture(t, "probe_available.json")); !ok || reason != "" {
		t.Fatalf("available probe = (%v, %q), want (true, \"\")", ok, reason)
	}
	ok, reason := decodeProbeResponse(fixture(t, "probe_unavailable.json"))
	if ok {
		t.Fatal("unavailable probe reported available")
	}
	if !contains(reason, "macOS 26") {
		t.Errorf("reason = %q, want the helper's reason", reason)
	}
	if ok, _ := decodeProbeResponse([]byte("not json")); ok {
		t.Error("unreadable probe must count as unavailable")
	}
	if ok, reason := decodeProbeResponse([]byte(`{"contract":9,"available":true}`)); ok || !contains(reason, "contract") {
		t.Errorf("mismatched contract probe = (%v, %q), want unavailable", ok, reason)
	}
	if ok, _ := decodeProbeResponse(fixture(t, "classify_error.json")); ok {
		t.Error("a structured error probe must count as unavailable")
	}
}

func TestHelperExitErrorPrefersStructuredError(t *testing.T) {
	err := helperExitError("kagaz-machelper", errors.New("exit status 3"), fixture(t, "classify_error.json"), "some noise")
	if !contains(err.Error(), "model_unavailable") {
		t.Fatalf("error = %q, want the structured error code", err)
	}
	err = helperExitError("kagaz-machelper", errors.New("exit status 3"), nil, "boom\nsecond line")
	if !contains(err.Error(), "boom") || contains(err.Error(), "second line") {
		t.Fatalf("error = %q, want only the first stderr line", err)
	}
}

// contains is a substring check kept local so the tests read as assertions.
func contains(haystack, needle string) bool {
	return len(needle) == 0 || len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
