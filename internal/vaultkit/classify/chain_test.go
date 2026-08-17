package classify

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getkagaz/kagaz/internal/vaultkit/config"
	"github.com/getkagaz/kagaz/internal/vaultkit/doctypes"
	"github.com/getkagaz/kagaz/internal/vaultkit/ocr"
)

// appleWith builds an Apple backend whose probe reports available and whose
// classify call replays the given recorded response.
func appleWith(t *testing.T, classifyFixture string, classifyErr error) *Apple {
	t.Helper()
	var out []byte
	if classifyFixture != "" {
		out = fixture(t, classifyFixture)
	}
	probe := fixture(t, "probe_available.json")
	return &Apple{
		locate: found,
		run: func(_ context.Context, _ string, args []string, _ string) ([]byte, error) {
			if isProbe(args) {
				return probe, nil
			}
			return out, classifyErr
		},
	}
}

// isProbe reports whether the argument list is a --probe invocation.
func isProbe(args []string) bool {
	for _, a := range args {
		if a == "--probe" {
			return true
		}
	}
	return false
}

// TestChainFallbackMatrix is the heart of this package: every way a semantic
// tier can be absent, wrong, slow or broken, and what the chain does about it.
func TestChainFallbackMatrix(t *testing.T) {
	cat := testCatalog(t)

	tests := []struct {
		name string
		// chain builds the chain under test.
		chain func(t *testing.T) *Chain
		text  string

		wantErr     string // non-empty means Classify must fail with this substring
		wantDocType string
		wantCat     string
		wantEngine  string
		wantZeroCnf bool
	}{
		{
			name: "auto with apple available uses apple",
			chain: func(t *testing.T) *Chain {
				return chainWith(cat, config.EngineAuto, appleWith(t, "classify_invoice.json", nil))
			},
			text:        invoiceText,
			wantDocType: "invoice",
			wantCat:     "financial",
			wantEngine:  config.EngineApple,
		},
		{
			name: "auto with apple absent degrades to rules",
			chain: func(t *testing.T) *Chain {
				return chainWith(cat, config.EngineAuto, &Apple{locate: missing, run: stubRunner(nil, errors.New("must not run"))})
			},
			text:        invoiceText,
			wantDocType: "invoice",
			wantCat:     "financial",
			wantEngine:  config.EngineRules,
		},
		{
			name: "auto with apple probing unavailable degrades to rules",
			chain: func(t *testing.T) *Chain {
				a := &Apple{locate: found, run: stubRunner(fixture(t, "probe_unavailable.json"), nil)}
				return chainWith(cat, config.EngineAuto, a)
			},
			text:        invoiceText,
			wantDocType: "invoice",
			wantEngine:  config.EngineRules,
		},
		{
			name: "apple returns a doctype outside the catalog",
			chain: func(t *testing.T) *Chain {
				return chainWith(cat, config.EngineAuto, appleWith(t, "classify_unknown_doctype.json", nil))
			},
			text:        invoiceText,
			wantDocType: "invoice",
			wantCat:     "financial",
			wantEngine:  config.EngineRules,
		},
		{
			// The escape hatch. The model is offered doctypes.Unclassified
			// alongside the catalog and answers that none of them fits; that is
			// a real answer, not the unknown-doctype failure it resembles, and
			// the deterministic tier still gets its turn.
			name: "apple declines: rules still get their turn",
			chain: func(t *testing.T) *Chain {
				return chainWith(cat, config.EngineAuto, appleWith(t, "classify_declined.json", nil))
			},
			text:        invoiceText,
			wantDocType: "invoice",
			wantCat:     "financial",
			wantEngine:  config.EngineRules,
		},
		{
			name: "apple declines and rules find nothing: unclassified, never a near miss",
			chain: func(t *testing.T) *Chain {
				return chainWith(cat, config.EngineAuto, appleWith(t, "classify_declined.json", nil))
			},
			text:        noiseText,
			wantDocType: doctypes.Unclassified,
			wantEngine:  config.EngineRules,
			wantZeroCnf: true,
		},
		{
			name: "apple below min_confidence",
			chain: func(t *testing.T) *Chain {
				return chainWith(cat, config.EngineAuto, appleWith(t, "classify_low_confidence.json", nil))
			},
			text:        invoiceText,
			wantDocType: "invoice",
			wantEngine:  config.EngineRules,
		},
		{
			name: "apple category disagreeing with the catalog: the catalog wins",
			chain: func(t *testing.T) *Chain {
				return chainWith(cat, config.EngineAuto, appleWith(t, "classify_wrong_category.json", nil))
			},
			text:        invoiceText,
			wantDocType: "invoice",
			wantCat:     "financial", // fixture says travel
			wantEngine:  config.EngineApple,
		},
		{
			name: "helper exits non-zero with a structured error",
			chain: func(t *testing.T) *Chain {
				return chainWith(cat, config.EngineAuto, appleWith(t, "", errors.New("kagaz-machelper: model unavailable (model_unavailable)")))
			},
			text:        invoiceText,
			wantDocType: "invoice",
			wantEngine:  config.EngineRules,
		},
		{
			name: "helper speaks an unknown contract version",
			chain: func(t *testing.T) *Chain {
				return chainWith(cat, config.EngineAuto, appleWith(t, "classify_bad_contract.json", nil))
			},
			text:        invoiceText,
			wantDocType: "invoice",
			wantEngine:  config.EngineRules,
		},
		{
			name: "helper emits malformed JSON",
			chain: func(t *testing.T) *Chain {
				return chainWith(cat, config.EngineAuto, appleWith(t, "classify_malformed.json", nil))
			},
			text:        invoiceText,
			wantDocType: "invoice",
			wantEngine:  config.EngineRules,
		},
		{
			name: "helper times out",
			chain: func(t *testing.T) *Chain {
				return chainWith(cat, config.EngineAuto, appleWith(t, "", context.DeadlineExceeded))
			},
			text:        invoiceText,
			wantDocType: "invoice",
			wantEngine:  config.EngineRules,
		},
		{
			name: "semantic fails and rules are also unconfident",
			chain: func(t *testing.T) *Chain {
				return chainWith(cat, config.EngineAuto, appleWith(t, "classify_error.json", nil))
			},
			text:        noiseText,
			wantDocType: doctypes.Unclassified,
			wantEngine:  config.EngineRules,
			wantZeroCnf: true,
		},
		{
			name: "engine rules never consults a model",
			chain: func(t *testing.T) *Chain {
				a := &Apple{locate: found, run: func(context.Context, string, []string, string) ([]byte, error) {
					t.Error("apple backend was invoked with engine=rules")
					return nil, errors.New("must not run")
				}}
				return chainWith(cat, config.EngineRules, a)
			},
			text:        invoiceText,
			wantDocType: "invoice",
			wantEngine:  config.EngineRules,
		},
		{
			name: "forced apple unavailable is an error naming the fix",
			chain: func(t *testing.T) *Chain {
				return chainWith(cat, config.EngineApple, &Apple{locate: missing})
			},
			text:    invoiceText,
			wantErr: "swift build --package-path machelper",
		},
		{
			name: "forced apple available but failing falls back to rules",
			chain: func(t *testing.T) *Chain {
				return chainWith(cat, config.EngineApple, appleWith(t, "", errors.New("helper blew up")))
			},
			text:        invoiceText,
			wantDocType: "invoice",
			wantEngine:  config.EngineRules,
		},
		{
			name: "unknown engine is an error",
			chain: func(t *testing.T) *Chain {
				return chainWith(cat, "telepathy", nil)
			},
			text:    invoiceText,
			wantErr: "telepathy",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := tc.chain(t)
			got, err := c.Classify(context.Background(), Request{Text: tc.text, Path: "/vault/doc.pdf"})

			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("Classify() = %+v, want an error containing %q", got, tc.wantErr)
				}
				if !contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %q, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Classify: %v", err)
			}
			if got.DocType != tc.wantDocType {
				t.Errorf("DocType = %q, want %q", got.DocType, tc.wantDocType)
			}
			if tc.wantCat != "" && got.Category != tc.wantCat {
				t.Errorf("Category = %q, want %q", got.Category, tc.wantCat)
			}
			if got.Engine != tc.wantEngine {
				t.Errorf("Engine = %q, want %q", got.Engine, tc.wantEngine)
			}
			if tc.wantZeroCnf && got.Confidence != 0 {
				t.Errorf("Confidence = %v, want 0", got.Confidence)
			}
			if !tc.wantZeroCnf && got.Confidence <= 0 {
				t.Errorf("Confidence = %v, want > 0", got.Confidence)
			}
		})
	}
}

// chainWith builds a chain around one Apple backend, with the other model tiers
// deliberately absent.
func chainWith(cat *doctypes.Catalog, engine string, apple *Apple) *Chain {
	return &Chain{
		Engine:        engine,
		MinConfidence: 0.5,
		Catalog:       cat,
		Rules:         &Rules{Catalog: cat},
		Apple:         apple,
	}
}

func TestChainAcceptedSemanticResultKeepsDeterministicFields(t *testing.T) {
	cat := testCatalog(t)
	c := chainWith(cat, config.EngineAuto, appleWith(t, "classify_invoice.json", nil))

	got, err := c.Classify(context.Background(), Request{Text: invoiceText})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if got.Engine != config.EngineApple {
		t.Fatalf("Engine = %q, want apple", got.Engine)
	}
	if got.Fields["invoice_number"] != "INV-2024-0912" {
		t.Errorf("invoice_number = %q, want the regex-extracted value", got.Fields["invoice_number"])
	}
	if got.Fields["vendor"] != "Acme Corporation" {
		t.Errorf("vendor = %q, want the model field to fill the gap", got.Fields["vendor"])
	}
	if got.Fields["amount"] != "4800.00" {
		t.Errorf("amount = %q, want the deterministic extraction to win", got.Fields["amount"])
	}
}

func TestChainWithoutCatalogFails(t *testing.T) {
	c := &Chain{Engine: config.EngineAuto}
	if _, err := c.Classify(context.Background(), Request{Text: invoiceText}); err == nil {
		t.Fatal("a chain with no catalog should report an error")
	}
}

func TestChainPassesCatalogSpecToTheHelper(t *testing.T) {
	cat := testCatalog(t)
	var gotSpec string
	var gotStdin string
	a := &Apple{
		locate: found,
		run: func(_ context.Context, _ string, args []string, stdin string) ([]byte, error) {
			if isProbe(args) {
				return fixture(t, "probe_available.json"), nil
			}
			for i, arg := range args {
				if arg == "--doctypes" && i+1 < len(args) {
					gotSpec = args[i+1]
				}
			}
			gotStdin = stdin
			return fixture(t, "classify_invoice.json"), nil
		},
	}
	c := chainWith(cat, config.EngineAuto, a)
	if _, err := c.Classify(context.Background(), Request{Text: invoiceText}); err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if gotSpec != cat.Spec() {
		t.Errorf("--doctypes = %q, want the resolved catalog spec", gotSpec)
	}
	if gotStdin != invoiceText {
		t.Errorf("stdin = %q, want the document text", gotStdin)
	}
	// The escape hatch is appended by whichever backend shows the list to a
	// model, never by the catalog: it has no category, so it has no place in a
	// "name:category" spec that the Go side also uses to resolve destinations.
	if contains(gotSpec, doctypes.Unclassified) {
		t.Errorf("--doctypes = %q, want it to carry only real, categorised doctypes", gotSpec)
	}
}

func TestChainForcedMLXUnavailableNamesModelPull(t *testing.T) {
	cat := testCatalog(t)
	c := &Chain{
		Engine:        config.EngineMLX,
		MinConfidence: 0.5,
		Catalog:       cat,
		Rules:         &Rules{Catalog: cat},
		MLX:           &MLX{Model: config.DefaultMLXModel, locate: missing},
	}
	_, err := c.Classify(context.Background(), Request{Text: invoiceText})
	if err == nil {
		t.Fatal("expected an error for a forced, uninstalled mlx engine")
	}
	if !contains(err.Error(), "kagaz model pull --engine mlx") {
		t.Fatalf("error = %q, want it to name the fix", err)
	}
}

func TestChainForcedBackendMissingEntirely(t *testing.T) {
	cat := testCatalog(t)
	c := &Chain{Engine: config.EngineOllama, MinConfidence: 0.5, Catalog: cat, Rules: &Rules{Catalog: cat}}
	if _, err := c.Classify(context.Background(), Request{Text: invoiceText}); err == nil {
		t.Fatal("a forced engine with no backend configured should error, not panic")
	}
}

func TestChainMLXEngineString(t *testing.T) {
	cat := testCatalog(t)
	m := &MLX{
		Model:  config.DefaultMLXModel,
		locate: func() (string, bool) { return "/fake/kagaz-machelper-mlx", true },
		run: func(_ context.Context, _ string, args []string, _ string) ([]byte, error) {
			if isProbe(args) {
				return fixture(t, "probe_available.json"), nil
			}
			return fixture(t, "classify_mlx_payslip.json"), nil
		},
	}
	c := &Chain{Engine: config.EngineMLX, MinConfidence: 0.5, Catalog: cat, Rules: &Rules{Catalog: cat}, MLX: m}

	got, err := c.Classify(context.Background(), Request{Text: "Payslip\nNet Pay: 1234.00\nPay Period: March 2024\n"})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if got.Engine != "mlx:Qwen2.5-3B-Instruct-4bit" {
		t.Fatalf("Engine = %q, want mlx:Qwen2.5-3B-Instruct-4bit", got.Engine)
	}
	if got.DocType != "payslip" || got.Category != "financial" {
		t.Fatalf("got %s/%s, want payslip/financial", got.DocType, got.Category)
	}
}

func TestAppleProbeIsCachedAndRaceSafe(t *testing.T) {
	var calls int32
	a := &Apple{
		locate: found,
		run: func(_ context.Context, _ string, args []string, _ string) ([]byte, error) {
			if isProbe(args) {
				atomic.AddInt32(&calls, 1)
			}
			return fixture(t, "probe_available.json"), nil
		},
	}

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !a.Available() {
				t.Error("Available() = false, want true")
			}
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("probe ran %d times, want exactly 1 (cached for the process lifetime)", got)
	}
}

func TestAppleUnavailableWithoutHelper(t *testing.T) {
	a := &Apple{locate: missing, run: stubRunner(nil, errors.New("must not run"))}
	if a.Available() {
		t.Fatal("Available() = true with no helper installed")
	}
	if !contains(a.detail(), "not found") {
		t.Errorf("detail() = %q, want it to explain the helper is missing", a.detail())
	}
	if _, err := a.Classify(context.Background(), Request{Text: invoiceText}); !errors.Is(err, ocr.ErrNoHelper) {
		t.Fatalf("Classify error = %v, want ocr.ErrNoHelper", err)
	}
}

func TestMLXWithoutModel(t *testing.T) {
	m := &MLX{locate: func() (string, bool) { return "/fake/kagaz-machelper-mlx", true }}
	if m.Available() {
		t.Fatal("Available() = true with no model configured")
	}
	if !contains(m.detail(), "classify.model") {
		t.Errorf("detail() = %q, want it to name the config key", m.detail())
	}
	if _, err := m.Classify(context.Background(), Request{Text: invoiceText}); err == nil {
		t.Fatal("Classify should fail without a model")
	}
}

func TestNewChainFromConfig(t *testing.T) {
	cfg, err := config.Parse([]byte("version: 1\nclassify:\n  engine: rules\n  min_confidence: 0.7\n"))
	if err != nil {
		t.Fatalf("config.Parse: %v", err)
	}
	cat := testCatalog(t)
	c := New(cfg, cat)

	if c.Engine != config.EngineRules {
		t.Errorf("Engine = %q, want rules", c.Engine)
	}
	if c.MinConfidence != 0.7 {
		t.Errorf("MinConfidence = %v, want 0.7", c.MinConfidence)
	}
	if c.MLX == nil || c.MLX.Model != config.DefaultMLXModel {
		t.Errorf("MLX model = %+v, want the configured default", c.MLX)
	}
	if c.Ollama == nil || c.Ollama.Endpoint != "http://localhost:11434" {
		t.Errorf("Ollama endpoint = %+v, want the configured localhost default", c.Ollama)
	}
	if !c.Available() || c.Name() != "chain" {
		t.Error("a chain is always available and is named chain")
	}
	if len(c.Describe()) == 0 {
		t.Error("Describe() returned nothing")
	}
}

// TestMinConfidenceZeroKeepsEveryMatch documents that a zero threshold means
// "accept any non-Unclassified answer", not "accept nothing".
func TestMinConfidenceZeroKeepsEveryMatch(t *testing.T) {
	cat := testCatalog(t)
	c := chainWith(cat, config.EngineAuto, appleWith(t, "classify_low_confidence.json", nil))
	c.MinConfidence = 0
	got, err := c.Classify(context.Background(), Request{Text: invoiceText})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if got.DocType != "passport" || got.Engine != config.EngineApple {
		t.Fatalf("got %s/%s, want the 0.31-confidence apple answer", got.DocType, got.Engine)
	}
}

// TestAppleProbeTimeoutIsNotCached is IMPORTANT 4.
//
// The first launch of a freshly installed, notarised Swift binary pays
// Gatekeeper verification and a cold dyld start. If that one slow probe were
// cached for the process lifetime, an entire ingest run would classify by rules
// and write `classifier: rules` into every sidecar with nothing to explain it.
func TestAppleProbeTimeoutIsNotCached(t *testing.T) {
	var calls int32
	a := &Apple{
		locate: found,
		run: func(context.Context, string, []string, string) ([]byte, error) {
			if atomic.AddInt32(&calls, 1) == 1 {
				return nil, context.DeadlineExceeded // cold start
			}
			return fixture(t, "probe_available.json"), nil
		},
	}

	if a.Available() {
		t.Fatal("first Available() = true, want false after a probe timeout")
	}
	if !a.Available() {
		t.Fatal("second Available() = false: a timeout must not be cached for the process lifetime")
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("probe ran %d times, want 2 (retry after a timeout)", got)
	}
}

// TestAppleDecodedUnavailableIsCached is the other half: a helper that ran and
// answered "no" is a settled fact and must not be re-probed per document.
func TestAppleDecodedUnavailableIsCached(t *testing.T) {
	var calls int32
	a := &Apple{
		locate: found,
		run: func(context.Context, string, []string, string) ([]byte, error) {
			atomic.AddInt32(&calls, 1)
			return fixture(t, "probe_unavailable.json"), nil
		},
	}
	for i := 0; i < 5; i++ {
		if a.Available() {
			t.Fatal("Available() = true, want false")
		}
	}
	if !contains(a.detail(), "macOS 26") {
		t.Errorf("detail() = %q, want the helper's reason", a.detail())
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("probe ran %d times, want exactly 1 (a decoded answer is cached)", got)
	}
}

// TestProbeTimeoutIsGenerous pins the budget: a tight probe timeout is how the
// whole-run silent downgrade happens in the first place.
func TestProbeTimeoutIsGenerous(t *testing.T) {
	if probeTimeout < 10*time.Second {
		t.Fatalf("probeTimeout = %v; a cold Gatekeeper-verified launch routinely exceeds a few seconds", probeTimeout)
	}
}

// TestMissingHelperIsNotCached lets a helper installed mid-run be noticed; the
// re-check costs one stat.
func TestMissingHelperIsNotCached(t *testing.T) {
	var installed bool
	a := &Apple{
		locate: func() (string, bool) {
			if installed {
				return "/fake/kagaz-machelper", true
			}
			return "", false
		},
		run: func(context.Context, string, []string, string) ([]byte, error) {
			return fixture(t, "probe_available.json"), nil
		},
	}
	if a.Available() {
		t.Fatal("Available() = true with no helper installed")
	}
	installed = true
	if !a.Available() {
		t.Fatal("Available() = false after the helper appeared: absence must not be cached")
	}
}

// TestChainDropsHallucinatedFields replays the exact Apple Foundation Models
// response that exposed the defect: a short business proposal containing no
// date and no reference number came back with date=2025-01-01 and
// document_number=12345, placeholder-shaped values invented to fill the
// schema. Those would have been written to the sidecar as facts about the
// user's document. The two fields that are really in the text must survive
// untouched -- a grounding check that strips real data is a worse defect than
// the one it fixes.
func TestChainDropsHallucinatedFields(t *testing.T) {
	const text = "BUSINESS PROPOSAL\nPrepared for: Hytron Metals\nPrepared by: Avvara Studio\n" +
		"Scope: automation of the recruitment pipeline.\nCommercials and phased delivery follow.\n"

	cat := testCatalog(t)
	c := chainWith(cat, config.EngineApple, appleWith(t, "classify_hallucinated_fields.json", nil))

	res, err := c.Classify(context.Background(), Request{Text: text, Catalog: cat})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if res.DocType != "proposal" {
		t.Fatalf("doctype = %q, want proposal", res.DocType)
	}
	for _, k := range []string{"issuer", "prepared for"} {
		if res.Fields[k] == "" {
			t.Errorf("field %q was dropped; it is written in the document text", k)
		}
	}
	for _, k := range []string{"date", "document_number"} {
		if v, ok := res.Fields[k]; ok {
			t.Errorf("field %q = %q survived; the document contains no such value", k, v)
		}
	}
	if len(res.Dropped) != 2 {
		t.Fatalf("Dropped = %+v, want the two fabricated fields recorded", res.Dropped)
	}
	for _, d := range res.Dropped {
		if d.Reason != ReasonUngrounded {
			t.Errorf("%s dropped with reason %q, want ReasonUngrounded", d.Field, d.Reason)
		}
	}
}

// TestChainKeepsGroundedFields is the regression the fix must not break: a
// genuine invoice keeps every real field, including the ones a model
// reformats -- an amount without its thousands separator and a date rewritten
// into ISO form.
func TestChainKeepsGroundedFields(t *testing.T) {
	const text = "TAX INVOICE / Invoice Number: INV-2026-4471 / Bill To: Alex Rao / " +
		"Acme Corp / Total: Rs. 4,800.00 / Due Date: 11 March 2026"

	cat := testCatalog(t)
	c := chainWith(cat, config.EngineApple, appleWith(t, "classify_hallucinated_fields.json", nil))
	c.Apple = &Apple{
		locate: found,
		run: func(_ context.Context, _ string, args []string, _ string) ([]byte, error) {
			if isProbe(args) {
				return fixture(t, "probe_available.json"), nil
			}
			return []byte(`{"contract":1,"engine":"apple","doctype":"invoice","category":"finance",` +
				`"confidence":0.9,"fields":{"invoice_number":"INV-2026-4471","bill_to":"alex rao",` +
				`"issuer":"Acme Corp","amount":"4800","due_date":"2026-03-11"}}`), nil
		},
	}

	res, err := c.Classify(context.Background(), Request{Text: text, Catalog: cat})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	// The model's own renderings, kept because the document really says them:
	// a lowercased name, and a name the catalog has no template for.
	want := map[string]string{
		"invoice_number": "INV-2026-4471",
		"bill_to":        "alex rao",
		"issuer":         "Acme Corp",
	}
	for k, v := range want {
		if got := res.Fields[k]; got != v {
			t.Errorf("field %q = %q, want %q kept: it is in the document, only rendered differently", k, got, v)
		}
	}
	// amount and due_date have catalog templates, so the regex capture off the
	// real text wins over the model's reformatting. Nothing is lost: the field
	// is present, with the document's own rendering.
	for k, v := range map[string]string{"amount": "4,800.00", "due_date": "11 March 2026"} {
		if got := res.Fields[k]; got != v {
			t.Errorf("field %q = %q, want the rules capture %q", k, got, v)
		}
	}
	for _, d := range res.Dropped {
		if d.Reason == ReasonUngrounded {
			t.Errorf("%s=%q was dropped as ungrounded, but the document says it", d.Field, d.Value)
		}
	}
}
