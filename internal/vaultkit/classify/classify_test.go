package classify

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/getkagaz/kagaz/internal/vaultkit/config"
	"github.com/getkagaz/kagaz/internal/vaultkit/doctypes"
	"github.com/getkagaz/kagaz/internal/vaultkit/ocr"
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
	const text = "Invoice from Acme for 4800.00, about 4800 all in."
	got, dropped := mergeFields(
		map[string]string{"amount": "4800.00"},
		map[string]string{"amount": "about 4800", "vendor": "Acme"},
		text,
	)
	if got["amount"] != "4800.00" {
		t.Errorf("amount = %q, want the deterministic value", got["amount"])
	}
	if got["vendor"] != "Acme" {
		t.Errorf("vendor = %q, want the model value to fill the gap", got["vendor"])
	}
	if len(dropped) != 1 || dropped[0].Field != "amount" || dropped[0].Reason != ReasonSuperseded {
		t.Errorf("dropped = %+v, want the model's amount superseded by the rules value", dropped)
	}
	if m, d := mergeFields(nil, nil, ""); m != nil || d != nil {
		t.Error("mergeFields(nil, nil) should be nil, not an empty map")
	}
	if m, _ := mergeFields(nil, map[string]string{" ": "x", "k": " "}, "x"); m != nil {
		t.Error("blank keys and values should be dropped, leaving nil")
	}
}

// TestMergeFieldsDropsUngrounded is the defect this file's grounding check
// exists for: the Apple backend returned a placeholder date and document
// number for a proposal containing neither.
func TestMergeFieldsDropsUngrounded(t *testing.T) {
	const text = "BUSINESS PROPOSAL\nPrepared for: Hytron Metals\nPrepared by: Avvara Studio\n"
	got, dropped := mergeFields(nil, map[string]string{
		"date":            "2025-01-01",
		"document_number": "12345",
		"issuer":          "Avvara Studio",
		"prepared for":    "Hytron Metals",
	}, text)

	for _, k := range []string{"issuer", "prepared for"} {
		if got[k] == "" {
			t.Errorf("%s was dropped; it is in the text and must survive", k)
		}
	}
	for _, k := range []string{"date", "document_number"} {
		if v, ok := got[k]; ok {
			t.Errorf("%s = %q survived; it appears nowhere in the text", k, v)
		}
	}
	if len(dropped) != 2 {
		t.Fatalf("dropped = %+v, want exactly the two fabricated fields", dropped)
	}
	for _, d := range dropped {
		if d.Reason != ReasonUngrounded {
			t.Errorf("%s dropped for %q, want the ungrounded reason", d.Field, d.Reason)
		}
	}
}

// TestGrounded is the hostile table. Every "want true" row is a formatting
// difference a model legitimately introduces; every "want false" row is
// something the document does not say.
func TestGrounded(t *testing.T) {
	const invoice = "TAX INVOICE / Invoice Number: INV-2026-4471 / Bill To: Alex Rao / " +
		"Acme Corp / Total: 4800.00 / Due Date: 11 March 2026"
	const proposal = "BUSINESS PROPOSAL\nPrepared for: Hytron Metals\nPrepared by: Avvara Studio\n" +
		"Scope: automation of the recruitment pipeline.\nCommercials and phased delivery follow.\n"

	tests := []struct {
		name string
		text string
		val  string
		want bool
	}{
		{"exact match", invoice, "INV-2026-4471", true},
		{"case difference", invoice, "acme corp", true},
		{"whitespace difference", "Bill To:\n  Alex   Rao\n", "Alex Rao", true},
		{"punctuation difference", invoice, "INV/2026/4471", true},
		{"thousands separator and decimals", "Amount due Rs. 4,800.00 only", "4800", true},
		{"decimals against a plain integer", "Total: 4800", "4,800.00", true},
		{"date normalised to ISO", invoice, "2026-03-11", true},
		{"date rendered long from ISO", "Dated 2026-03-11.", "11 March 2026", true},
		{"abbreviated month", "Issued 11 Mar 2026", "2026-03-11", true},
		{"month-first rendering", "March 11, 2026", "2026-03-11", true},
		{"two-digit year", "Paid 11/03/26", "2026-03-11", true},
		{"ambiguous numeric date, other reading", "Paid 03/11/2026", "2026-03-11", true},

		{"fabricated date", proposal, "2025-01-01", false},
		{"fabricated document number", proposal, "12345", false},
		{"fabricated date against a real invoice", invoice, "2025-01-01", false},
		{"fabricated number against a real invoice", invoice, "12345", false},
		{"wrong year, right month and day", invoice, "2019-03-11", false},
		{"wrong day", invoice, "2026-03-12", false},
		{"number that is not in the text", invoice, "4900", false},
		{"tokens present but not adjacent", "Acme is here. Corp is elsewhere.", "Acme Corp", false},
		{"words wrapped round a real number", invoice, "invoice 12345", false},
		{"partial token", invoice, "Acme Corporation", false},
		{"empty value", invoice, "", false},
		{"blank value", invoice, "   ", false},
		{"empty text", "", "Acme", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Grounded(tc.text, tc.val); got != tc.want {
				t.Errorf("Grounded(%q) = %v, want %v", tc.val, got, tc.want)
			}
		})
	}
}

// TestGroundedUsesUnclippedText proves a value that only appears past maxText
// is still grounded: the model is shown a clipped copy, but the check runs
// against Request.Text in full.
func TestGroundedUsesUnclippedText(t *testing.T) {
	text := strings.Repeat("a ", maxText) + "Zephyr Holdings"
	got, dropped := mergeFields(nil, map[string]string{"issuer": "Zephyr Holdings"}, text)
	if got["issuer"] != "Zephyr Holdings" {
		t.Errorf("issuer = %q (dropped %+v), want the value grounded in the clipped-away tail",
			got["issuer"], dropped)
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
		{name: "unknown contract", file: "classify_bad_contract.json", wantErr: "helper speaks contract 2"},
		{name: "malformed json", file: "classify_malformed.json", wantErr: "decoding response"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := decodeClassifyResponse(ocr.HelperBinary, "apple", fixture(t, tc.file))
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
		_, err := decodeClassifyResponse(ocr.HelperBinary, "apple", []byte(`{"contract":1,"doctype":"  "}`))
		if err == nil || !contains(err.Error(), "no doctype") {
			t.Fatalf("error = %v, want a no-doctype error", err)
		}
	})
}

func TestDecodeProbeResponse(t *testing.T) {
	if ok, reason, code := decodeProbeResponse(fixture(t, "probe_available.json")); !ok || reason != "" || code != "" {
		t.Fatalf("available probe = (%v, %q, %q), want (true, \"\", \"\")", ok, reason, code)
	}
	ok, reason, code := decodeProbeResponse(fixture(t, "probe_unavailable.json"))
	if ok {
		t.Fatal("unavailable probe reported available")
	}
	if !contains(reason, "macOS 26") {
		t.Errorf("reason = %q, want the helper's reason", reason)
	}
	// The fixture predates reason_code, which is exactly the case an older
	// helper presents: prose, and an honest "unknown" rather than a guess.
	if code != ReasonUnknown {
		t.Errorf("code = %q, want %q for a helper that sends none", code, ReasonUnknown)
	}
	if ok, _, code := decodeProbeResponse([]byte("not json")); ok || code != ReasonUnreadableProbe {
		t.Errorf("unreadable probe = (%v, %q), want unavailable/%s", ok, code, ReasonUnreadableProbe)
	}
	if ok, reason, code := decodeProbeResponse([]byte(`{"contract":9,"available":true}`)); ok ||
		!contains(reason, "contract") || code != ReasonContractMismatch {
		t.Errorf("mismatched contract probe = (%v, %q, %q), want unavailable", ok, reason, code)
	}
	if ok, _, _ := decodeProbeResponse(fixture(t, "classify_error.json")); ok {
		t.Error("a structured error probe must count as unavailable")
	}
	// A helper that does send a code has it passed through verbatim: the
	// vocabulary is the helper's to extend, and Go must not re-derive it from
	// the prose.
	if ok, _, code := decodeProbeResponse(
		[]byte(`{"contract":1,"engine":"mlx","available":false,"reason":"no weights","reason_code":"weights_missing"}`)); ok ||
		code != ReasonWeightsMissing {
		t.Errorf("probe with a code = (%v, %q), want unavailable/%s", ok, code, ReasonWeightsMissing)
	}
}

func TestHelperFailurePrefersStructuredError(t *testing.T) {
	err := helperFailure(ocr.HelperBinary, errors.New("exit status 3"), fixture(t, "classify_error.json"), "some noise")
	var failure *ocr.HelperFailure
	if !errors.As(err, &failure) {
		t.Fatalf("error is %T, want *ocr.HelperFailure", err)
	}
	if failure.Code != "model_unavailable" {
		t.Fatalf("Code = %q, want model_unavailable", failure.Code)
	}

	err = helperFailure(ocr.HelperBinary, errors.New("exit status 3"), nil, "boom\nsecond line")
	if !errors.As(err, &failure) {
		t.Fatalf("error is %T, want *ocr.HelperFailure", err)
	}
	if failure.Code != "" || failure.Message != "boom" {
		t.Fatalf("failure = %+v, want no code and only the first stderr line", failure)
	}
}

// TestDecodeFailuresCarryDistinguishableCodes is what lets `kagaz doctor`
// tell a refusing model from a hung helper from a version mismatch.
func TestDecodeFailuresCarryDistinguishableCodes(t *testing.T) {
	tests := map[string]struct {
		data     []byte
		wantCode string
	}{
		"structured error": {fixture(t, "classify_error.json"), "model_unavailable"},
		"unknown contract": {fixture(t, "classify_bad_contract.json"), CodeUnsupportedContract},
		"malformed json":   {fixture(t, "classify_malformed.json"), CodeBadResponse},
		"missing doctype":  {[]byte(`{"contract":1,"doctype":""}`), CodeBadResponse},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := decodeClassifyResponse(ocr.HelperBinary, "apple", tc.data)
			var failure *ocr.HelperFailure
			if !errors.As(err, &failure) {
				t.Fatalf("error is %T, want *ocr.HelperFailure", err)
			}
			if failure.Code != tc.wantCode {
				t.Fatalf("Code = %q, want %q", failure.Code, tc.wantCode)
			}
		})
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
