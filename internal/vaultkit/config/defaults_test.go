package config

import (
	"strconv"
	"strings"
	"testing"
)

// TestDefaultStructureGivesEverySharedLabel pins the fix for the defect that a
// vault built from the defaults could not file a single unowned document:
// only `identity` carried a shared label, so conventions.Render's deliberate
// refusal to invent an owner fired for every other category — including
// `company`, where third-party documents (a client's certificate, an
// incorporation document) overwhelmingly land.
func TestDefaultStructureGivesEverySharedLabel(t *testing.T) {
	for name, cat := range DefaultStructure() {
		if cat.Shared == "" {
			t.Errorf("structure.%s has no shared label; an unowned document in this category cannot be filed", name)
		}
		if cat.Shared != DefaultSharedFolder {
			t.Errorf("structure.%s.shared = %q, want %q", name, cat.Shared, DefaultSharedFolder)
		}
		if cat.Path == "" {
			t.Errorf("structure.%s has no path", name)
		}
		if cat.Layout == "" {
			t.Errorf("structure.%s has no layout", name)
		}
	}
}

// TestDefaultCategoriesMatchesDefaultStructure keeps the rendering order used
// by `kagaz init` in step with the map it renders: a category present in one
// and not the other would be written into a user's vault.yaml incompletely or
// not at all.
func TestDefaultCategoriesMatchesDefaultStructure(t *testing.T) {
	s := DefaultStructure()
	names := DefaultCategories()
	if len(names) != len(s) {
		t.Fatalf("DefaultCategories has %d names, DefaultStructure has %d categories", len(names), len(s))
	}
	seen := map[string]bool{}
	for _, n := range names {
		if seen[n] {
			t.Errorf("DefaultCategories lists %q twice", n)
		}
		seen[n] = true
		if _, ok := s[n]; !ok {
			t.Errorf("DefaultCategories lists %q, which DefaultStructure does not define", n)
		}
	}
	for n := range s {
		if !seen[n] {
			t.Errorf("DefaultStructure defines %q, which DefaultCategories omits", n)
		}
	}
}

// TestMinimalConfigDefaultsToASharedLabelEverywhere covers the path a real
// vault takes: a vault.yaml with no `structure:` block at all must still come
// out of Parse with a shared label on every category.
func TestMinimalConfigDefaultsToASharedLabelEverywhere(t *testing.T) {
	cfg, err := Parse([]byte("version: 1\nvault_root: .\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.Structure) == 0 {
		t.Fatal("no structure was defaulted")
	}
	for name, cat := range cfg.Structure {
		if cat.Shared == "" {
			t.Errorf("structure.%s.shared is empty after defaulting", name)
		}
	}
}

// TestExplicitStructureKeepsAnEmptySharedLabel guards the other half of the
// contract: defaulting fills in a shared label only when the vault supplies no
// structure at all. A vault that names a category and deliberately leaves
// `shared` off wants Render's refusal, and must keep it.
func TestExplicitStructureKeepsAnEmptySharedLabel(t *testing.T) {
	cfg, err := Parse([]byte("version: 1\nvault_root: .\nstructure:\n  company:\n    path: Company\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := cfg.Structure["company"].Shared; got != "" {
		t.Fatalf("an explicitly declared category gained shared=%q; the refusal path is no longer reachable", got)
	}
}

// TestClassifyEngineDefaultsToApple pins the default engine. apple needs
// nothing downloaded and nothing running -- where Apple's on-device model is
// missing the chain answers from rules -- which is what makes it the one
// engine safe to default to.
func TestClassifyEngineDefaultsToApple(t *testing.T) {
	c := &Config{}
	c.applyDefaults()
	if c.Classify.Engine != EngineApple {
		t.Errorf("classify.engine = %q, want %q", c.Classify.Engine, EngineApple)
	}
	if err := c.Validate(); err != nil {
		t.Errorf("the defaults must validate: %v", err)
	}
}

// TestClassifyEngineAutoIsRejected pins that the retired value fails loudly.
// "auto" chose a tier without saying which; reading it as "apple" now would be
// exactly the silent rewrite of a user's config that naming the four engines
// was meant to end. The message has to say what to put instead.
func TestClassifyEngineAutoIsRejected(t *testing.T) {
	c := &Config{}
	c.applyDefaults()
	c.Classify.Engine = "auto"
	err := c.Validate()
	if err == nil {
		t.Fatal("classify.engine: auto must be rejected")
	}
	for _, want := range []string{EngineApple, EngineMLX, EngineOllama, EngineRules} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

// TestClassifyModelHasNoDefault pins the half of the split that a fixture
// cannot: naming an Ollama model the user did not choose would put that name
// in doctor output and in every sidecar's provenance, so the key stays empty
// and the ollama tier reports itself unconfigured instead.
func TestClassifyModelHasNoDefault(t *testing.T) {
	c := &Config{}
	c.applyDefaults()
	if c.Classify.Model != "" {
		t.Errorf("classify.model defaulted to %q, want empty", c.Classify.Model)
	}
	if err := c.Validate(); err != nil {
		t.Errorf("the defaults must validate: %v", err)
	}
}

// TestClassifyModelRejectsRepoPaths pins the "/" discriminator. The value that
// trips it is almost always the repo path this key used to default to, left in
// a vault written when classify.model fed the mlx engine too; handing it to
// Ollama would 404 on every document instead of saying what is wrong.
func TestClassifyModelRejectsRepoPaths(t *testing.T) {
	bad := []string{
		DefaultMLXModel,
		"mlx-community/Llama-3.2-3B-Instruct-4bit",
		"org/name",
	}
	for _, v := range bad {
		c := &Config{}
		c.applyDefaults()
		c.Classify.Model = v
		err := c.Validate()
		if err == nil {
			t.Errorf("classify.model %q was accepted, want a rejection", v)
			continue
		}
		for _, want := range []string{"classify.model", strconv.Quote(v), EngineOllama, DefaultMLXModel, "qwen2.5:3b"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not contain %q", err, want)
			}
		}
	}

	// Legitimate Ollama names, tagged and untagged, must pass untouched: the
	// check must not become a general "looks odd to me" filter.
	for _, v := range []string{"", "qwen2.5:3b", "llama3.1:8b", "mistral", "my-model:v1.2", "hf.co_qwen:latest"} {
		c := &Config{}
		c.applyDefaults()
		c.Classify.Model = v
		if err := c.Validate(); err != nil {
			t.Errorf("classify.model %q was rejected: %v", v, err)
		}
	}
}

// TestOCROllamaDefaultsToOff pins the opt-in. A vault.yaml with no `ocr:` block
// must not send document images to a local daemon, so the filled-in value is
// asserted against DefaultOCROllamaEnabled rather than against a literal typed
// twice -- a literal here would keep passing if the default were flipped back
// and the constant left behind.
func TestOCROllamaDefaultsToOff(t *testing.T) {
	if DefaultOCROllamaEnabled != OCROllamaOff {
		t.Fatalf("DefaultOCROllamaEnabled = %q; an omitted ocr.ollama.enabled must not opt the vault in", DefaultOCROllamaEnabled)
	}

	// Both routes into a Config: the in-memory zero value and a real parse.
	c := &Config{}
	c.applyDefaults()
	if c.OCR.Ollama.Enabled != DefaultOCROllamaEnabled {
		t.Errorf("applyDefaults: ocr.ollama.enabled = %q, want %q", c.OCR.Ollama.Enabled, DefaultOCROllamaEnabled)
	}
	if err := c.Validate(); err != nil {
		t.Errorf("the defaults must validate: %v", err)
	}

	parsed, err := Parse([]byte("people:\n  - name: Alex Rao\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if parsed.OCR.Ollama.Enabled != DefaultOCROllamaEnabled {
		t.Errorf("parsed vault with no ocr block: ocr.ollama.enabled = %q, want %q",
			parsed.OCR.Ollama.Enabled, DefaultOCROllamaEnabled)
	}
}

// TestOCROllamaTriStateRoundTrips keeps the three accepted values accepted.
// Defaulting to off narrows what an *omitted* key means and nothing else:
// "auto" is still a value the user can choose, and an explicit "false" must
// survive defaulting rather than be treated as absent.
func TestOCROllamaTriStateRoundTrips(t *testing.T) {
	for _, want := range validOCROllamaEnabled {
		yaml := "people:\n  - name: Alex Rao\nocr:\n  ollama:\n    enabled: \"" + want + "\"\n"
		c, err := Parse([]byte(yaml))
		if err != nil {
			t.Fatalf("Parse(enabled: %q): %v", want, err)
		}
		if c.OCR.Ollama.Enabled != want {
			t.Errorf("ocr.ollama.enabled = %q, want %q", c.OCR.Ollama.Enabled, want)
		}
		if err := c.Validate(); err != nil {
			t.Errorf("Validate(enabled: %q): %v", want, err)
		}
	}
}

// TestOCROllamaRejectsUnknownValues pins that the tri-state is still closed and
// that the rejection names the field and the value. "yes", "on" and "1" are the
// plausible guesses; reading any of them as an opt-in would be the same silent
// rewrite of a user's config the classify.engine check exists to prevent.
func TestOCROllamaRejectsUnknownValues(t *testing.T) {
	for _, bad := range []string{"yes", "on", "1", "TRUE", "enabled", "  auto"} {
		c := &Config{}
		c.applyDefaults()
		c.OCR.Ollama.Enabled = bad
		err := c.Validate()
		if err == nil {
			t.Errorf("ocr.ollama.enabled %q was accepted, want a rejection", bad)
			continue
		}
		for _, want := range []string{"ocr.ollama.enabled", strconv.Quote(bad), OCROllamaAuto, OCROllamaOn, OCROllamaOff} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not contain %q", err, want)
			}
		}
	}
}
