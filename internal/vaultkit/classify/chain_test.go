package classify

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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
			name: "apple with the tier available uses it",
			chain: func(t *testing.T) *Chain {
				return chainWith(cat, config.EngineApple, appleWith(t, "classify_invoice.json", nil))
			},
			text:        invoiceText,
			wantDocType: "invoice",
			wantCat:     "financial",
			wantEngine:  config.EngineApple,
		},
		{
			name: "apple with the tier absent degrades to rules",
			chain: func(t *testing.T) *Chain {
				return chainWith(cat, config.EngineApple, &Apple{locate: missing, run: stubRunner(nil, errors.New("must not run"))})
			},
			text:        invoiceText,
			wantDocType: "invoice",
			wantCat:     "financial",
			wantEngine:  config.EngineRules,
		},
		{
			name: "apple probing unavailable degrades to rules",
			chain: func(t *testing.T) *Chain {
				a := &Apple{locate: found, run: stubRunner(fixture(t, "probe_unavailable.json"), nil)}
				return chainWith(cat, config.EngineApple, a)
			},
			text:        invoiceText,
			wantDocType: "invoice",
			wantEngine:  config.EngineRules,
		},
		{
			name: "apple returns a doctype outside the catalog",
			chain: func(t *testing.T) *Chain {
				return chainWith(cat, config.EngineApple, appleWith(t, "classify_unknown_doctype.json", nil))
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
				return chainWith(cat, config.EngineApple, appleWith(t, "classify_declined.json", nil))
			},
			text:        invoiceText,
			wantDocType: "invoice",
			wantCat:     "financial",
			wantEngine:  config.EngineRules,
		},
		{
			name: "apple declines and rules find nothing: unclassified, never a near miss",
			chain: func(t *testing.T) *Chain {
				return chainWith(cat, config.EngineApple, appleWith(t, "classify_declined.json", nil))
			},
			text:        noiseText,
			wantDocType: doctypes.Unclassified,
			wantEngine:  config.EngineRules,
			wantZeroCnf: true,
		},
		{
			name: "apple below min_confidence",
			chain: func(t *testing.T) *Chain {
				return chainWith(cat, config.EngineApple, appleWith(t, "classify_low_confidence.json", nil))
			},
			text:        invoiceText,
			wantDocType: "invoice",
			wantEngine:  config.EngineRules,
		},
		{
			name: "apple category disagreeing with the catalog: the catalog wins",
			chain: func(t *testing.T) *Chain {
				return chainWith(cat, config.EngineApple, appleWith(t, "classify_wrong_category.json", nil))
			},
			text:        invoiceText,
			wantDocType: "invoice",
			wantCat:     "financial", // fixture says travel
			wantEngine:  config.EngineApple,
		},
		{
			name: "helper exits non-zero with a structured error",
			chain: func(t *testing.T) *Chain {
				return chainWith(cat, config.EngineApple, appleWith(t, "", errors.New("kagaz-machelper: model unavailable (model_unavailable)")))
			},
			text:        invoiceText,
			wantDocType: "invoice",
			wantEngine:  config.EngineRules,
		},
		{
			name: "helper speaks an unknown contract version",
			chain: func(t *testing.T) *Chain {
				return chainWith(cat, config.EngineApple, appleWith(t, "classify_bad_contract.json", nil))
			},
			text:        invoiceText,
			wantDocType: "invoice",
			wantEngine:  config.EngineRules,
		},
		{
			name: "helper emits malformed JSON",
			chain: func(t *testing.T) *Chain {
				return chainWith(cat, config.EngineApple, appleWith(t, "classify_malformed.json", nil))
			},
			text:        invoiceText,
			wantDocType: "invoice",
			wantEngine:  config.EngineRules,
		},
		{
			name: "helper times out",
			chain: func(t *testing.T) *Chain {
				return chainWith(cat, config.EngineApple, appleWith(t, "", context.DeadlineExceeded))
			},
			text:        invoiceText,
			wantDocType: "invoice",
			wantEngine:  config.EngineRules,
		},
		{
			name: "semantic fails and rules are also unconfident",
			chain: func(t *testing.T) *Chain {
				return chainWith(cat, config.EngineApple, appleWith(t, "classify_error.json", nil))
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
			// apple is the DEFAULT engine, and Apple's on-device model does
			// not exist before macOS 26. Erroring here would make Kagaz
			// unusable out of the box on every other machine, so this one
			// degrades. mlx and ollama do not: see
			// TestInstalledOnPurposeEnginesErrorWhenAbsent.
			name: "apple unavailable degrades to rules rather than failing",
			chain: func(t *testing.T) *Chain {
				return chainWith(cat, config.EngineApple, &Apple{locate: missing})
			},
			text:        invoiceText,
			wantDocType: "invoice",
			wantEngine:  config.EngineRules,
		},
		{
			name: "apple available but failing falls back to rules",
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
	c := chainWith(cat, config.EngineApple, appleWith(t, "classify_invoice.json", nil))

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
	c := &Chain{Engine: config.EngineApple}
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
	c := chainWith(cat, config.EngineApple, a)
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
		MLX:           &MLX{locate: missing},
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

// TestMLXIgnoresClassifyModel is the regression guard for the change that
// split the two engines' settings: classify.model is Ollama's, and the MLX
// tier must reach the helper with the pinned repo no matter what a vault says.
// It asserts on the --model argument actually handed to the helper, for both
// the probe and the classification, because an assertion on a struct field
// would have passed in the version this replaced.
func TestMLXIgnoresClassifyModel(t *testing.T) {
	cfg, err := config.Parse([]byte("version: 1\nclassify:\n  engine: mlx\n  model: \"not-a-real-model:9000\"\n"))
	if err != nil {
		t.Fatalf("config.Parse: %v", err)
	}
	cat := testCatalog(t)
	c := New(cfg, cat)

	var got []string
	c.MLX.locate = func() (string, bool) { return "/fake/kagaz-machelper-mlx", true }
	c.MLX.run = func(_ context.Context, _ string, args []string, _ string) ([]byte, error) {
		for i, a := range args {
			if a == "--model" && i+1 < len(args) {
				got = append(got, args[i+1])
			}
		}
		if isProbe(args) {
			return fixture(t, "probe_available.json"), nil
		}
		return fixture(t, "classify_mlx_payslip.json"), nil
	}

	res, err := c.Classify(context.Background(), Request{Text: "Payslip\nNet Pay: 1234.00\n"})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("--model seen %d times (%v), want once for the probe and once for the run", len(got), got)
	}
	for _, m := range got {
		if m != config.DefaultMLXModel {
			t.Errorf("helper --model = %q, want the pinned %q", m, config.DefaultMLXModel)
		}
	}
	if res.Engine != config.EngineMLX+":"+modelBasename(config.DefaultMLXModel) {
		t.Errorf("Result.Engine = %q, want the pinned model's basename", res.Engine)
	}
	if !contains(c.MLX.detail(), config.DefaultMLXModel) {
		t.Errorf("detail() = %q, want it to name the pinned model", c.MLX.detail())
	}
}

// TestMLXPinnedWithoutAnyClassifyBlock: a vault.yaml with no classify: key at
// all must still produce a pinned MLX tier, not one with an empty model.
func TestMLXPinnedWithoutAnyClassifyBlock(t *testing.T) {
	cfg, err := config.Parse([]byte("version: 1\n"))
	if err != nil {
		t.Fatalf("config.Parse: %v", err)
	}
	if cfg.Classify.Model != "" {
		t.Errorf("classify.model defaulted to %q; it must stay empty", cfg.Classify.Model)
	}
	c := New(cfg, testCatalog(t))
	if c.MLX.engine() != config.EngineMLX+":"+modelBasename(config.DefaultMLXModel) {
		t.Errorf("MLX engine = %q, want the pinned model", c.MLX.engine())
	}
}

// TestOllamaTierReadsClassifyModel is the other half: the key that MLX stopped
// reading must still reach Ollama, and an unset one must be reported through
// the structured vocabulary rather than guessed at.
func TestOllamaTierReadsClassifyModel(t *testing.T) {
	cfg, err := config.Parse([]byte("version: 1\nclassify:\n  engine: ollama\n  model: \"qwen2.5:3b\"\n"))
	if err != nil {
		t.Fatalf("config.Parse: %v", err)
	}
	if got := New(cfg, testCatalog(t)).Ollama.Model; got != "qwen2.5:3b" {
		t.Errorf("Ollama.Model = %q, want the configured qwen2.5:3b", got)
	}

	cfg, err = config.Parse([]byte("version: 1\nclassify:\n  engine: ollama\n"))
	if err != nil {
		t.Fatalf("config.Parse: %v", err)
	}
	o := New(cfg, testCatalog(t)).Ollama
	if o.Model != "" {
		t.Fatalf("Ollama.Model = %q, want empty: no model may be guessed", o.Model)
	}
	if o.Available() {
		t.Error("Available() = true with no model configured")
	}
	if got := o.reason(); got != ReasonModelNotConfigured {
		t.Errorf("reason() = %q, want %q", got, ReasonModelNotConfigured)
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
	if c.MLX == nil || c.MLX.engine() != config.EngineMLX+":"+modelBasename(config.DefaultMLXModel) {
		t.Errorf("MLX model = %+v, want the pinned default", c.MLX)
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
	c := chainWith(cat, config.EngineApple, appleWith(t, "classify_low_confidence.json", nil))
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

// mlxWith builds an MLX backend whose probe reports available and whose
// classify call replays the given recorded response.
func mlxWith(t *testing.T, classifyFixture string, classifyErr error) *MLX {
	t.Helper()
	var out []byte
	if classifyFixture != "" {
		out = fixture(t, classifyFixture)
	}
	probe := fixture(t, "probe_available.json")
	return &MLX{
		locate: found,
		run: func(_ context.Context, _ string, args []string, _ string) ([]byte, error) {
			if isProbe(args) {
				return probe, nil
			}
			return out, classifyErr
		},
	}
}

// mlxEngine is the provenance string an accepted MLX answer must carry.
var mlxEngine = config.EngineMLX + ":" + modelBasename(config.DefaultMLXModel)

// ollamaServing builds an Ollama backend backed by a loopback test server that
// reports the model pulled and answers every generation with answer. Nothing
// leaves the machine: httptest listens on 127.0.0.1. calls, when non-nil,
// counts generations.
func ollamaServing(t *testing.T, answer ollamaAnswer, calls *atomic.Int64) *Ollama {
	t.Helper()
	body, err := json.Marshal(answer)
	if err != nil {
		t.Fatalf("marshalling answer: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"models": []map[string]string{{"name": "qwen2.5:3b", "model": "qwen2.5:3b"}},
			})
		case "/api/generate":
			if calls != nil {
				calls.Add(1)
			}
			_ = json.NewEncoder(w).Encode(ollamaResponse{Response: string(body), Done: true})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return &Ollama{Endpoint: srv.URL, Model: "qwen2.5:3b", client: srv.Client()}
}

// chainAll builds a chain over all three model tiers. Any of them may be nil,
// which means "not present on this machine".
func chainAll(cat *doctypes.Catalog, engine string, apple *Apple, mlx *MLX, ollama *Ollama) *Chain {
	return &Chain{
		Engine:        engine,
		MinConfidence: 0.5,
		Catalog:       cat,
		Rules:         &Rules{Catalog: cat},
		Apple:         apple,
		MLX:           mlx,
		Ollama:        ollama,
	}
}

// declinedOllama is an Ollama answer that uses the escape hatch.
var declinedOllama = ollamaAnswer{DocType: doctypes.Unclassified, Confidence: 0}

// payslipOllama is an accepted Ollama answer for a doctype the rules tier would
// not pick from invoiceText, so provenance is unambiguous.
var payslipOllama = ollamaAnswer{DocType: "payslip", Category: "travel", Confidence: 0.91}

// TestChainRulesEngineRunsNoModelAndTakesNoProbe pins the explicit "no LLM
// used" choice: not one helper is launched, and not even an availability probe
// is taken.
func TestChainRulesEngineRunsNoModelAndTakesNoProbe(t *testing.T) {
	cat := testCatalog(t)
	forbid := func(_ context.Context, _ string, args []string, _ string) ([]byte, error) {
		t.Errorf("a helper was invoked with engine=rules: %v", args)
		return nil, errors.New("must not run")
	}
	locateForbid := func() (string, bool) {
		t.Error("helper discovery ran with engine=rules")
		return "", false
	}
	o := &Ollama{
		Endpoint: "http://localhost:11434",
		Model:    "qwen2.5:3b",
		client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Error("the ollama daemon was contacted with engine=rules")
			return nil, errors.New("must not run")
		})},
	}
	c := chainAll(cat, config.EngineRules,
		&Apple{locate: locateForbid, run: forbid},
		&MLX{locate: locateForbid, run: forbid}, o)

	got, err := c.Classify(context.Background(), Request{Text: invoiceText})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if got.Engine != config.EngineRules || got.DocType != "invoice" {
		t.Errorf("got %s/%s, want invoice/rules", got.DocType, got.Engine)
	}
	if plan := c.Plan(); len(plan.Order) != 1 || plan.Order[0] != config.EngineRules {
		t.Errorf("Plan().Order = %v, want [rules]", plan.Order)
	}
}

// TestEachEngineRunsItsOwnTierThenRules pins the four engines. Falling back to
// the deterministic tier is part of each model engine's definition, so there is
// no "mlx or nothing": mlx declining lands on rules exactly as apple does.
func TestEachEngineRunsItsOwnTierThenRules(t *testing.T) {
	cat := testCatalog(t)

	tests := []struct {
		name        string
		chain       func(t *testing.T) *Chain
		wantDocType string
		wantEngine  string
	}{
		{
			name: "apple answers",
			chain: func(t *testing.T) *Chain {
				return chainAll(cat, config.EngineApple, appleWith(t, "classify_invoice.json", nil), nil, nil)
			},
			wantDocType: "invoice",
			wantEngine:  config.EngineApple,
		},
		{
			name: "apple declines, rules answer",
			chain: func(t *testing.T) *Chain {
				return chainAll(cat, config.EngineApple, appleWith(t, "classify_declined.json", nil), nil, nil)
			},
			wantDocType: "invoice",
			wantEngine:  config.EngineRules,
		},
		{
			name: "mlx answers",
			chain: func(t *testing.T) *Chain {
				return chainAll(cat, config.EngineMLX, nil, mlxWith(t, "classify_mlx_payslip.json", nil), nil)
			},
			wantDocType: "payslip",
			wantEngine:  mlxEngine,
		},
		{
			name: "mlx declines, rules answer",
			chain: func(t *testing.T) *Chain {
				return chainAll(cat, config.EngineMLX, nil, mlxWith(t, "classify_declined.json", nil), nil)
			},
			wantDocType: "invoice",
			wantEngine:  config.EngineRules,
		},
		{
			name: "mlx fails, rules answer",
			chain: func(t *testing.T) *Chain {
				return chainAll(cat, config.EngineMLX, nil, mlxWith(t, "", errors.New("helper blew up")), nil)
			},
			wantDocType: "invoice",
			wantEngine:  config.EngineRules,
		},
		{
			name: "ollama answers",
			chain: func(t *testing.T) *Chain {
				return chainAll(cat, config.EngineOllama, nil, nil, ollamaServing(t, payslipOllama, nil))
			},
			wantDocType: "payslip",
			wantEngine:  "ollama:qwen2.5:3b",
		},
		{
			name: "ollama declines, rules answer",
			chain: func(t *testing.T) *Chain {
				return chainAll(cat, config.EngineOllama, nil, nil, ollamaServing(t, declinedOllama, nil))
			},
			wantDocType: "invoice",
			wantEngine:  config.EngineRules,
		},
		{
			// No engine named at all: the config default, which is apple.
			name: "an empty engine is apple",
			chain: func(t *testing.T) *Chain {
				return chainAll(cat, "", appleWith(t, "classify_invoice.json", nil), nil, nil)
			},
			wantDocType: "invoice",
			wantEngine:  config.EngineApple,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.chain(t).Classify(context.Background(), Request{Text: invoiceText, Path: "/vault/doc.pdf"})
			if err != nil {
				t.Fatalf("Classify: %v", err)
			}
			if got.DocType != tc.wantDocType || got.Engine != tc.wantEngine {
				t.Errorf("got %s/%s, want %s/%s", got.DocType, got.Engine, tc.wantDocType, tc.wantEngine)
			}
		})
	}
}

// TestNoEngineReachesAnotherModelTier pins that each engine is its own tier and
// rules, and never another model: a machine with all three installed classifies
// exactly like one with only the engine that was named.
func TestNoEngineReachesAnotherModelTier(t *testing.T) {
	cat := testCatalog(t)

	for _, engine := range []string{config.EngineApple, config.EngineMLX, config.EngineOllama} {
		t.Run(engine, func(t *testing.T) {
			var appleCalls, mlxCalls, ollamaCalls atomic.Int64
			probe := fixture(t, "probe_available.json")
			declined := fixture(t, "classify_declined.json")
			a := &Apple{locate: found, run: func(_ context.Context, _ string, args []string, _ string) ([]byte, error) {
				if isProbe(args) {
					return probe, nil
				}
				appleCalls.Add(1)
				return declined, nil
			}}
			m := &MLX{locate: found,
				run: func(_ context.Context, _ string, args []string, _ string) ([]byte, error) {
					if isProbe(args) {
						return probe, nil
					}
					mlxCalls.Add(1)
					return declined, nil
				}}
			c := chainAll(cat, engine, a, m, ollamaServing(t, declinedOllama, &ollamaCalls))

			got, err := c.Classify(context.Background(), Request{Text: invoiceText})
			if err != nil {
				t.Fatalf("Classify: %v", err)
			}
			if got.Engine != config.EngineRules {
				t.Errorf("Engine = %q, want rules", got.Engine)
			}
			for _, tc := range []struct {
				name string
				n    int64
			}{
				{config.EngineApple, appleCalls.Load()},
				{config.EngineMLX, mlxCalls.Load()},
				{config.EngineOllama, ollamaCalls.Load()},
			} {
				want := int64(0)
				if tc.name == engine {
					want = 1
				}
				if tc.n != want {
					t.Errorf("%s classified %d times under engine=%s, want %d", tc.name, tc.n, engine, want)
				}
			}
		})
	}
}

// TestFieldExtractionIsUnconditional pins that the catalog's regexes run over
// every accepted answer, whichever tier produced it. No setting can switch them
// off, which is what keeps invoice_number and amount in the sidecar.
func TestFieldExtractionIsUnconditional(t *testing.T) {
	cat := testCatalog(t)
	for _, c := range []*Chain{
		chainAll(cat, config.EngineApple, appleWith(t, "classify_invoice.json", nil), nil, nil),
		chainAll(cat, config.EngineRules, nil, nil, nil),
	} {
		got, err := c.Classify(context.Background(), Request{Text: invoiceText})
		if err != nil {
			t.Fatalf("engine %s: Classify: %v", c.Engine, err)
		}
		if got.Fields["invoice_number"] != "INV-2024-0912" {
			t.Errorf("engine %s: invoice_number = %q, want the regex-extracted value", c.Engine, got.Fields["invoice_number"])
		}
		if got.Fields["amount"] != "4800.00" {
			t.Errorf("engine %s: amount = %q, want the deterministic extraction", c.Engine, got.Fields["amount"])
		}
	}
}

// TestEngineAutoIsRejected pins that the retired value is an error rather than
// a quiet alias for apple. A config file that says "auto" was written when auto
// meant something, and rewriting its meaning behind the user's back is what the
// four named engines exist to stop.
func TestEngineAutoIsRejected(t *testing.T) {
	cat := testCatalog(t)
	c := chainAll(cat, "auto", appleWith(t, "classify_invoice.json", nil), nil, nil)
	_, err := c.Classify(context.Background(), Request{Text: invoiceText})
	if err == nil {
		t.Fatal("engine \"auto\" must be rejected, not treated as apple")
	}
	for _, want := range []string{config.EngineApple, config.EngineMLX, config.EngineOllama, config.EngineRules} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

// TestInstalledOnPurposeEnginesErrorWhenAbsent pins the one hard failure. A
// user who wrote classify.engine: mlx installed weights on purpose; quietly
// filing by keyword instead would misreport provenance in every sidecar, so
// kagaz refuses and names the fix. apple is exempt because it is the default
// and cannot be installed at all (see the matrix above).
func TestInstalledOnPurposeEnginesErrorWhenAbsent(t *testing.T) {
	cat := testCatalog(t)

	tests := map[string]struct {
		chain   *Chain
		wantFix string
	}{
		config.EngineMLX: {
			chain:   chainAll(cat, config.EngineMLX, nil, &MLX{locate: missing}, nil),
			wantFix: "kagaz model pull",
		},
		config.EngineOllama: {
			chain:   chainAll(cat, config.EngineOllama, nil, nil, &Ollama{Endpoint: "http://localhost:1", Model: "qwen2.5:3b"}),
			wantFix: "ollama",
		},
	}
	for engine, tc := range tests {
		t.Run(engine, func(t *testing.T) {
			_, err := tc.chain.Classify(context.Background(), Request{Text: invoiceText})
			if err == nil {
				t.Fatal("an unavailable, explicitly installed engine must be an error")
			}
			if !strings.Contains(err.Error(), tc.wantFix) {
				t.Errorf("error %q does not name the fix (%q)", err, tc.wantFix)
			}
		})
	}
}

// TestChainPlanReportsTheOrderItWillActuallyTry pins what `kagaz doctor` shows,
// which the Settings window prints verbatim rather than recomputing.
func TestChainPlanReportsTheOrderItWillActuallyTry(t *testing.T) {
	cat := testCatalog(t)

	// The default engine, with mlx and ollama installed and ready: still
	// apple -> rules, and they are not reported as skipped, which would
	// suggest apple would otherwise have used them.
	ready := chainAll(cat, config.EngineApple,
		appleWith(t, "classify_invoice.json", nil),
		mlxWith(t, "classify_declined.json", nil),
		ollamaServing(t, declinedOllama, nil))
	plan := ready.Plan()
	if plan.Engine != config.EngineApple {
		t.Errorf("Engine = %q, want apple", plan.Engine)
	}
	if len(plan.Order) != 2 || plan.Order[0] != config.EngineApple || plan.Order[1] != config.EngineRules {
		t.Errorf("Order = %v, want [apple rules]", plan.Order)
	}
	if len(plan.Skipped) != 0 {
		t.Errorf("Skipped = %v, want none", plan.Skipped)
	}
	if plan.Err != "" {
		t.Errorf("Err = %q, want none", plan.Err)
	}

	// apple missing: it is reported as skipped, and rules still answer.
	without := chainAll(cat, config.EngineApple,
		&Apple{locate: missing, run: stubRunner(nil, errors.New("must not run"))}, nil, nil)
	p := without.Plan()
	if len(p.Order) != 1 || p.Order[0] != config.EngineRules {
		t.Errorf("Order = %v, want [rules]", p.Order)
	}
	if len(p.Skipped) != 1 || p.Skipped[0] != config.EngineApple {
		t.Errorf("Skipped = %v, want [apple]", p.Skipped)
	}

	// mlx, ready: mlx -> rules.
	named := chainAll(cat, config.EngineMLX, nil, mlxWith(t, "classify_declined.json", nil), nil)
	if got := named.Plan(); len(got.Order) != 2 || got.Order[0] != config.EngineMLX || got.Order[1] != config.EngineRules {
		t.Errorf("Order = %v, want [mlx rules]", got.Order)
	}

	// An uninstalled mlx is the one condition doctor must show as a failure
	// rather than an order.
	bad := chainAll(cat, config.EngineMLX, nil, &MLX{locate: missing}, nil)
	if p := bad.Plan(); p.Err == "" {
		t.Errorf("Plan() = %+v, want an error for an uninstalled mlx", p)
	}
}

// TestDescribeReportsWhichPreconditionIsUnmet pins the machine-readable half of
// `kagaz doctor`'s classifier checks.
//
// MLX has three independent preconditions -- the helper binary, its Metal
// shader library, and the weights -- and only the weights are fixed by
// `kagaz model pull`. In prose they are three similar sentences; a client that
// offered a 1.6 GB download for the wrong one would make the user wait minutes
// for nothing to change. So the code is reported alongside the prose, and this
// test is what stops the two drifting apart.
func TestDescribeReportsWhichPreconditionIsUnmet(t *testing.T) {
	cat := testCatalog(t)

	mlxProbing := func(f string) *MLX {
		out := fixture(t, f)
		return &MLX{locate: found,
			run: func(_ context.Context, _ string, args []string, _ string) ([]byte, error) {
				if isProbe(args) {
					return out, nil
				}
				return nil, errors.New("must not classify")
			}}
	}

	tests := []struct {
		name  string
		chain *Chain
		want  map[string]string // engine -> reason code
	}{
		{
			name:  "the helper binary is not installed",
			chain: chainAll(cat, config.EngineMLX, nil, &MLX{locate: missing}, nil),
			want:  map[string]string{config.EngineMLX: ReasonHelperMissing},
		},
		{
			name:  "the helper is there and the weights are not",
			chain: chainAll(cat, config.EngineMLX, nil, mlxProbing("probe_weights_missing.json"), nil),
			want:  map[string]string{config.EngineMLX: ReasonWeightsMissing},
		},
		{
			name:  "the helper is there and its shader library is not",
			chain: chainAll(cat, config.EngineMLX, nil, mlxProbing("probe_shader_library_missing.json"), nil),
			want:  map[string]string{config.EngineMLX: ReasonShaderLibraryMissing},
		},
		{
			name:  "an available tier reports no reason at all",
			chain: chainAll(cat, config.EngineMLX, nil, mlxWith(t, "classify_mlx_payslip.json", nil), nil),
			want:  map[string]string{config.EngineMLX: ""},
		},
		{
			name:  "no ollama daemon",
			chain: chainAll(cat, config.EngineOllama, nil, nil, &Ollama{Endpoint: "http://127.0.0.1:1", Model: "qwen2.5:3b"}),
			want:  map[string]string{config.EngineOllama: ReasonDaemonUnreachable},
		},
		{
			name:  "an ollama daemon without the model",
			chain: chainAll(cat, config.EngineOllama, nil, nil, ollamaWithoutModel(t)),
			want:  map[string]string{config.EngineOllama: ReasonModelNotPulled},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := map[string]string{}
			for _, s := range tc.chain.Describe() {
				got[s.Name] = s.Reason
			}
			for engine, want := range tc.want {
				if got[engine] != want {
					t.Errorf("%s reason = %q, want %q", engine, got[engine], want)
				}
			}
			// The rules tier is always available and never carries a reason.
			if got[config.EngineRules] != "" {
				t.Errorf("rules reason = %q, want none", got[config.EngineRules])
			}
		})
	}
}

// ollamaWithoutModel is a daemon that answers but has not pulled the model.
func ollamaWithoutModel(t *testing.T) *Ollama {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			_ = json.NewEncoder(w).Encode(map[string]any{"models": []map[string]string{}})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return &Ollama{Endpoint: srv.URL, Model: "qwen2.5:3b", client: srv.Client()}
}
