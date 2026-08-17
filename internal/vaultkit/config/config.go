// Package config parses and validates vault.yaml, the single authored source of
// truth for a Kagaz vault. Nothing in Kagaz hardcodes conventions: folder names,
// filename grammar, fiscal-year math, tag vocabulary and the doctype catalog all
// resolve from here.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// FileName is the conventional name of the vault configuration file.
const FileName = "vault.yaml"

// SchemaVersion is the highest vault.yaml `version:` this build understands.
const SchemaVersion = 1

// Config is a fully parsed, defaulted and validated vault.yaml.
type Config struct {
	Version int `yaml:"version"`
	// Name is an optional human label for this vault, e.g. "Personal & Family
	// KYC". It is display-only: it names the vault in INDEX.md, AGENTS.md and
	// `kagaz doctor`, and it never contributes to a path, a folder name, a
	// filename, a manifest path or a staging path. Read it through DisplayName,
	// which falls back to the vault_root folder name when it is unset.
	Name       string       `yaml:"name,omitempty"`
	VaultRoot  string       `yaml:"vault_root"`
	FiscalYear FiscalYear   `yaml:"fiscal_year"`
	People     []Person     `yaml:"people"`
	OwnerGroup OwnerGroup   `yaml:"owner_groups"`
	Filename   Filename     `yaml:"filename"`
	Structure  Structure    `yaml:"structure"`
	Tags       Tags         `yaml:"tags"`
	OCR        OCR          `yaml:"ocr"`
	Classify   Classify     `yaml:"classify"`
	Encrypted  Encrypted    `yaml:"encrypted_docs"`
	Lint       Lint         `yaml:"lint"`
	Confidence Confidential `yaml:"confidential"`
	DocTypes   []DocType    `yaml:"doctypes"`

	// Path is the absolute path vault.yaml was loaded from. Not serialized.
	Path string `yaml:"-"`
}

// FiscalYear describes how calendar dates map onto fiscal periods. A start month
// of 1 means the fiscal year is the calendar year, which is the global default;
// 4 (India, Japan, UK-ish), 7 (Australia) and 10 (US federal) are the common
// split-year alternatives.
type FiscalYear struct {
	StartMonth  int    `yaml:"start_month"`
	LabelFormat string `yaml:"label_format"`
}

// Person is a vault owner. Name is the display form used in filenames and
// folders; Tag is the lowercase slug used for Finder tags and CLI filters.
type Person struct {
	Name string `yaml:"name"`
	Tag  string `yaml:"tag"`
}

// OwnerGroup controls how documents belonging to several people are addressed.
type OwnerGroup struct {
	SeparatorFolder   string `yaml:"separator_folder"`
	SeparatorFilename string `yaml:"separator_filename"`
	Order             string `yaml:"order"`
}

// Filename is the filename grammar. Pattern uses {Field} placeholders with
// optional [...] segments, e.g. "{DocType}_{Names}_{Identifier}[_{Year}]".
type Filename struct {
	Pattern       string `yaml:"pattern"`
	WordSeparator string `yaml:"word_separator"`
	FieldSep      string `yaml:"field_separator"`
}

// Structure maps doctype categories onto folders beneath the vault root.
type Structure map[string]Category

// Category is one top-level area of the vault.
//
// Layout is a slash-separated template of {Owner} and {FY} segments describing
// the subtree beneath Path. Shared, when set, names the folder used instead of
// {Owner} for documents owned by more than one person.
type Category struct {
	Path   string `yaml:"path"`
	Shared string `yaml:"shared,omitempty"`
	Layout string `yaml:"layout,omitempty"`
}

// Tags is the controlled vocabulary. Anything outside it is a lint violation,
// which is what keeps Finder tags searchable rather than a free-text swamp.
type Tags struct {
	Companies   []string `yaml:"companies"`
	Areas       []string `yaml:"areas"`
	FiscalYears []string `yaml:"fiscal_years"`
	Lifecycle   []string `yaml:"lifecycle"`
}

// OCR configures text extraction.
type OCR struct {
	VisionLanguages []string  `yaml:"vision_languages"`
	Ollama          OCROllama `yaml:"ollama"`
}

// OCROllama is the opt-in Ollama OCR runner. Enabled is a tri-state string:
// "auto", "true" or "false".
type OCROllama struct {
	Enabled  string `yaml:"enabled"`
	Model    string `yaml:"model"`
	Endpoint string `yaml:"endpoint"`
}

// Classify selects and tunes the semantic classifier backend.
type Classify struct {
	Engine        string  `yaml:"engine"`
	Model         string  `yaml:"model"`
	Endpoint      string  `yaml:"endpoint"`
	MinConfidence float64 `yaml:"min_confidence"`
}

// Encrypted controls handling of password-protected documents.
//
// NOT IMPLEMENTED. Both fields are parsed and validated -- so a vault.yaml
// written today stays valid -- but nothing consumes them: Kagaz has no
// encrypted-document handling, and internal/vaultkit/keychain, its intended
// home, is imported by nothing. Do not document either field as working.
type Encrypted struct {
	// KeepEncrypted is a tri-state: nil means the key was absent from
	// vault.yaml, which is distinct from an explicit false. The safe default
	// is true -- Kagaz must never be the reason a document loses its
	// encryption -- but "leave it encrypted" is also a setting a user can
	// legitimately turn off, and a plain bool cannot represent both. With a
	// plain bool, defaulting the zero value to true would silently overwrite
	// an explicit `keep_encrypted: false` (as written in examples/vault.yaml
	// and the fixture vault) and defaulting it to false would fail open on an
	// omitted key. Use KeepEncryptedDocs to read the effective value; do not
	// read this field directly.
	KeepEncrypted *bool  `yaml:"keep_encrypted"`
	PasswordStore string `yaml:"password_store"`
}

// KeepEncryptedDocs reports the effective value of
// encrypted_docs.keep_encrypted: true when the key was absent from vault.yaml
// (fail safe -- an omitted key must never mean "strip the encryption"), or the
// explicit value otherwise. This is the only supported way to read the
// setting.
func (e *Encrypted) KeepEncryptedDocs() bool {
	if e.KeepEncrypted == nil {
		return true
	}
	return *e.KeepEncrypted
}

// Lint enables individual convention checks.
type Lint struct {
	RequireLifecycleTag             bool     `yaml:"require_lifecycle_tag"`
	SingleActivePerDocTypePerPerson []string `yaml:"single_active_per_doctype_per_person"`
	ForbidPasswordsInFilenames      bool     `yaml:"forbid_passwords_in_filenames"`
}

// Confidential governs the external-send gate.
type Confidential struct {
	// RequireConfirmationOnResolveForSend is a tri-state: nil means the key
	// was absent from vault.yaml, distinct from an explicit false. A plain
	// bool cannot make that distinction (its zero value is indistinguishable
	// from an explicit false), which would silently force the gate to a
	// fixed value regardless of what a user wrote. Use ConfirmationRequired
	// to read the effective value; do not read this field directly.
	RequireConfirmationOnResolveForSend *bool  `yaml:"require_confirmation_on_resolve_for_send"`
	AuditLog                            string `yaml:"audit_log"`
}

// ConfirmationRequired reports the effective value of
// require_confirmation_on_resolve_for_send: true when the key was absent
// from vault.yaml (fail closed), or the explicit value otherwise. This is
// the only supported way to read the setting.
func (c *Confidential) ConfirmationRequired() bool {
	if c.RequireConfirmationOnResolveForSend == nil {
		return true
	}
	return *c.RequireConfirmationOnResolveForSend
}

// DocType is a per-vault extension or override of the built-in catalog.
type DocType struct {
	Name     string            `yaml:"name"`
	Category string            `yaml:"category"`
	Match    DocTypeMatch      `yaml:"match"`
	Extract  map[string]string `yaml:"extract"`
}

// DocTypeMatch holds the high-precision rules used by the offline fallback
// classifier. Keywords match on word boundaries; Patterns are Go regexps.
type DocTypeMatch struct {
	Keywords []string `yaml:"keywords"`
	Patterns []string `yaml:"patterns"`
}

// Valid classifier engines.
const (
	EngineAuto   = "auto"
	EngineApple  = "apple"
	EngineMLX    = "mlx"
	EngineOllama = "ollama"
	EngineRules  = "rules"
)

var validEngines = []string{EngineAuto, EngineApple, EngineMLX, EngineOllama, EngineRules}

// DefaultMLXModel is the pinned MLX weights repository. It is a text LLM on
// purpose: vision-language loaders take a different MLX code path.
const DefaultMLXModel = "mlx-community/Qwen2.5-3B-Instruct-4bit"

// ErrNotFound is returned by Find and Load when no vault.yaml is present.
var ErrNotFound = errors.New("no vault.yaml found")

// Find walks up from start looking for a vault.yaml, returning its path. An
// empty start means the current working directory.
func Find(start string) (string, error) {
	if start == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		start = wd
	}
	dir, err := filepath.Abs(ExpandHome(start))
	if err != nil {
		return "", err
	}
	// A directly-named vault.yaml is accepted as-is.
	if filepath.Base(dir) == FileName {
		if _, err := os.Stat(dir); err == nil {
			return dir, nil
		}
	}
	for {
		candidate := filepath.Join(dir, FileName)
		if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
			return candidate, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", ErrNotFound
		}
		dir = parent
	}
}

// Load finds, parses, defaults and validates the vault config for start.
func Load(start string) (*Config, error) {
	path, err := Find(start)
	if err != nil {
		return nil, err
	}
	return LoadFile(path)
}

// LoadFile parses, defaults and validates a specific vault.yaml.
func LoadFile(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	cfg.Path = path
	// A relative vault_root resolves against the directory holding vault.yaml,
	// so a vault stays portable when moved wholesale.
	if !filepath.IsAbs(cfg.VaultRoot) {
		cfg.VaultRoot = filepath.Join(filepath.Dir(path), cfg.VaultRoot)
	}
	cfg.VaultRoot = filepath.Clean(cfg.VaultRoot)
	return cfg, nil
}

// Parse decodes YAML into a Config, applies defaults and validates it.
func Parse(data []byte) (*Config, error) {
	var cfg Config
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// ExpandHome replaces a leading ~ with the user's home directory.
func ExpandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return p
		}
		return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
	}
	return p
}

func (c *Config) applyDefaults() {
	if c.Version == 0 {
		c.Version = SchemaVersion
	}
	// Name is deliberately *not* defaulted to the vault_root folder name here.
	// Defaulting it would put a value into the in-memory Config that the user
	// never wrote, and anything that ever serialises a Config back would then
	// silently plant a `name:` in a hand-edited vault.yaml. The fallback lives
	// in DisplayName instead, where it costs nothing and cannot be written out.
	// Trimming is not defaulting: it invents no value, it only stops trailing
	// whitespace from turning into a mis-rendered heading.
	c.Name = strings.TrimSpace(c.Name)
	if c.VaultRoot == "" {
		c.VaultRoot = "~/Documents"
	}
	c.VaultRoot = ExpandHome(c.VaultRoot)

	if c.FiscalYear.StartMonth == 0 {
		c.FiscalYear.StartMonth = 1
	}
	if c.FiscalYear.LabelFormat == "" {
		if c.FiscalYear.StartMonth == 1 {
			c.FiscalYear.LabelFormat = "FY {yyyy1}"
		} else {
			c.FiscalYear.LabelFormat = "FY {yy1}-{yy2}"
		}
	}

	if c.OwnerGroup.SeparatorFolder == "" {
		c.OwnerGroup.SeparatorFolder = "+"
	}
	if c.OwnerGroup.SeparatorFilename == "" {
		// "+" rather than "-": the owner separator must differ from
		// filename.word_separator, or the grammar stops being invertible.
		// With both set to "-", "Alex-Rao" reads equally well as the single
		// person "Alex Rao" and as the two owners "Alex" and "Rao", and
		// nothing in the filename can settle it -- so Kagaz has to guess
		// where the document belongs. Matching separator_folder makes
		// "Alex-Rao" one person and "Alex+Sam" two, by construction.
		// A vault may still configure "-" explicitly; conventions.Parse
		// resolves that ambiguity against people:, and lint declines to
		// assert a destination when it cannot.
		c.OwnerGroup.SeparatorFilename = "+"
	}
	if c.OwnerGroup.Order == "" {
		c.OwnerGroup.Order = "alphabetical"
	}

	if c.Filename.Pattern == "" {
		c.Filename.Pattern = "{DocType}_{Names}_{Identifier}[_{Year}][_{Modifier}]"
	}
	if c.Filename.WordSeparator == "" {
		c.Filename.WordSeparator = "-"
	}
	if c.Filename.FieldSep == "" {
		c.Filename.FieldSep = "_"
	}

	if len(c.Structure) == 0 {
		c.Structure = DefaultStructure()
	}
	for name, cat := range c.Structure {
		if cat.Path == "" {
			cat.Path = strings.Title(name) //nolint:staticcheck // ASCII category names only
		}
		if cat.Layout == "" {
			cat.Layout = defaultLayout(name)
		}
		c.Structure[name] = cat
	}

	if len(c.Tags.Lifecycle) == 0 {
		c.Tags.Lifecycle = DefaultLifecycleTags()
	}

	if len(c.OCR.VisionLanguages) == 0 {
		c.OCR.VisionLanguages = []string{"en-US"}
	}
	if c.OCR.Ollama.Enabled == "" {
		c.OCR.Ollama.Enabled = "auto"
	}
	if c.OCR.Ollama.Endpoint == "" {
		c.OCR.Ollama.Endpoint = "http://localhost:11434"
	}

	if c.Classify.Engine == "" {
		c.Classify.Engine = EngineAuto
	}
	if c.Classify.Model == "" {
		c.Classify.Model = DefaultMLXModel
	}
	if c.Classify.Endpoint == "" {
		c.Classify.Endpoint = "http://localhost:11434"
	}
	if c.Classify.MinConfidence == 0 {
		c.Classify.MinConfidence = 0.5
	}

	if c.Encrypted.PasswordStore == "" {
		c.Encrypted.PasswordStore = "keychain"
	}
	// No default is set here for KeepEncrypted, for the same reason as
	// RequireConfirmationOnResolveForSend below: it stays nil when vault.yaml
	// omits the key, and the fail-safe "nil means true" logic lives in
	// Encrypted.KeepEncryptedDocs so that an explicit `false` survives
	// defaulting instead of being silently overwritten.
	if c.Confidence.AuditLog == "" {
		c.Confidence.AuditLog = "vault.log"
	}
	// No default is set here for RequireConfirmationOnResolveForSend: it
	// stays nil when vault.yaml omits the key, and the fail-closed "nil
	// means true" logic lives in Confidential.ConfirmationRequired, not
	// here, specifically so an explicit `false` in vault.yaml survives
	// defaulting instead of being silently overwritten.

	for i, p := range c.People {
		if p.Tag == "" {
			c.People[i].Tag = Slug(p.Name)
		}
	}
}

// DefaultLifecycleTags is the built-in lifecycle vocabulary.
func DefaultLifecycleTags() []string {
	return []string{"active", "superseded", "encrypted", "confidential", "to-action", "dont-touch"}
}

// DefaultSharedFolder is the folder every default category uses in place of
// {Owner} for documents that belong to more than one person or to nobody.
//
// Every category carries one, not just identity: a third party's document — a
// client's incorporation certificate, a supplier's insurance schedule — has no
// owner to infer, and conventions.Render deliberately refuses to invent a name
// for it. Without a shared label on the category, that refusal is the only
// outcome, so a vault built from these defaults could not file an unowned
// document at all. A vault that wants the refusal back can clear `shared:` on
// the category it cares about.
const DefaultSharedFolder = "_Shared"

// DefaultStructure is the global-first category-to-folder mapping used when
// vault.yaml does not specify one.
func DefaultStructure() Structure {
	s := Structure{
		"personal":  {Path: "Personal"},
		"company":   {Path: "Company"},
		"financial": {Path: "Financial"},
		"travel":    {Path: "Travel"},
		"identity":  {Path: "Identity"},
		"insurance": {Path: "Insurance"},
		"medical":   {Path: "Medical"},
		"legal":     {Path: "Legal"},
		"property":  {Path: "Property"},
		"vehicles":  {Path: "Vehicles"},
		"utility":   {Path: "Utilities"},
	}
	for name, cat := range s {
		cat.Layout = defaultLayout(name)
		cat.Shared = DefaultSharedFolder
		s[name] = cat
	}
	return s
}

// DefaultCategories lists the default category names in the order they are
// presented to a user. Callers that render the structure block need a stable
// order; ranging over DefaultStructure gives a different one every time.
func DefaultCategories() []string {
	return []string{
		"personal", "company", "financial", "travel", "identity",
		"insurance", "medical", "legal", "property", "vehicles", "utility",
	}
}

// defaultLayout partitions the categories that accumulate one document per
// period by fiscal year, and leaves the rest as a flat per-owner folder.
func defaultLayout(category string) string {
	switch category {
	case "financial", "company", "utility":
		return "{Owner}/{FY}"
	default:
		return "{Owner}"
	}
}

// Validate reports the first structural problem with the config.
func (c *Config) Validate() error {
	if c.Version > SchemaVersion {
		return fmt.Errorf("vault.yaml version %d is newer than this build supports (%d); upgrade kagaz", c.Version, SchemaVersion)
	}
	if err := ValidateName(c.Name); err != nil {
		return err
	}
	if c.FiscalYear.StartMonth < 1 || c.FiscalYear.StartMonth > 12 {
		return fmt.Errorf("fiscal_year.start_month must be 1-12, got %d", c.FiscalYear.StartMonth)
	}
	if !strings.Contains(c.FiscalYear.LabelFormat, "{") {
		return fmt.Errorf("fiscal_year.label_format %q contains no placeholder", c.FiscalYear.LabelFormat)
	}
	if c.Filename.WordSeparator == c.Filename.FieldSep {
		return errors.New("filename.word_separator and filename.field_separator must differ")
	}
	if !strings.Contains(c.Filename.Pattern, "{DocType}") {
		return errors.New("filename.pattern must contain {DocType}")
	}

	seenTag := map[string]string{}
	for _, p := range c.People {
		if p.Name == "" {
			return errors.New("people: entry with empty name")
		}
		if prev, dup := seenTag[p.Tag]; dup {
			return fmt.Errorf("people: %q and %q share tag %q", prev, p.Name, p.Tag)
		}
		seenTag[p.Tag] = p.Name
	}

	if len(c.Structure) == 0 {
		return errors.New("structure: at least one category is required")
	}
	for name, cat := range c.Structure {
		if cat.Path == "" {
			return fmt.Errorf("structure.%s.path is empty", name)
		}
		if filepath.IsAbs(cat.Path) || strings.Contains(cat.Path, "..") {
			return fmt.Errorf("structure.%s.path %q must be a relative path inside the vault", name, cat.Path)
		}
		for _, seg := range strings.Split(cat.Layout, "/") {
			switch seg {
			case "{Owner}", "{FY}", "":
			default:
				return fmt.Errorf("structure.%s.layout: unknown segment %q (want {Owner} or {FY})", name, seg)
			}
		}
	}

	if !contains(validEngines, c.Classify.Engine) {
		return fmt.Errorf("classify.engine %q must be one of %s", c.Classify.Engine, strings.Join(validEngines, ", "))
	}
	if c.Classify.MinConfidence < 0 || c.Classify.MinConfidence > 1 {
		return fmt.Errorf("classify.min_confidence must be 0-1, got %v", c.Classify.MinConfidence)
	}
	if err := requireLocalhost(c.Classify.Endpoint); err != nil {
		return fmt.Errorf("classify.endpoint: %w", err)
	}
	if err := requireLocalhost(c.OCR.Ollama.Endpoint); err != nil {
		return fmt.Errorf("ocr.ollama.endpoint: %w", err)
	}
	switch c.OCR.Ollama.Enabled {
	case "auto", "true", "false":
	default:
		return fmt.Errorf("ocr.ollama.enabled must be auto, true or false, got %q", c.OCR.Ollama.Enabled)
	}

	for _, dt := range c.DocTypes {
		if dt.Name == "" {
			return errors.New("doctypes: entry with empty name")
		}
		if dt.Category == "" {
			return fmt.Errorf("doctypes.%s: category is required", dt.Name)
		}
		if _, ok := c.Structure[dt.Category]; !ok {
			return fmt.Errorf("doctypes.%s: category %q is not defined in structure", dt.Name, dt.Category)
		}
	}
	return nil
}

// MaxNameLen is the longest vault name accepted, in runes. The name is a label
// rendered into a Markdown heading, a doctor row and (eventually) a GUI list;
// past this length it stops being a label and starts being a paragraph.
const MaxNameLen = 80

// DisplayName is the human label for this vault: `name:` when vault.yaml sets
// one, otherwise the folder name of vault_root. It is the only supported way to
// read the name, and it is display-only — see the warning on ValidateName.
//
// The absolute vault root is never returned: it is specific to one machine, and
// INDEX.md and AGENTS.md must stay byte-identical across a sync.
func (c *Config) DisplayName() string {
	if c.Name != "" {
		return c.Name
	}
	base := filepath.Base(filepath.Clean(c.VaultRoot))
	if base == "." || base == string(filepath.Separator) || base == "" {
		return "vault"
	}
	return base
}

// ValidateName reports whether a string is acceptable as a vault `name:`. It is
// exported so a caller that is about to *write* a vault.yaml (`kagaz init
// --name`) can reject a bad name before the file exists, rather than writing a
// file that then fails to load.
//
// The name is a human label and nothing else. **No path-building code may ever
// read Config.Name or DisplayName** — not a destination folder, a filename, a
// manifest path or a staging path. That structural rule, not this function, is
// what makes a name safe; the checks here are a second line, cheap enough to be
// worth having in a tool whose whole job is moving files:
//
//   - Path separators and the `..` element are rejected outright, so a name can
//     never be a traversal payload even if some future caller forgets the rule.
//   - Control characters (including newline and tab) are rejected because the
//     name is interpolated into a Markdown heading and printed to a terminal: a
//     newline breaks the heading, and an ESC could forge terminal output.
//   - The length is bounded, so the name cannot swamp the output it labels.
func ValidateName(name string) error {
	if name == "" {
		return nil
	}
	if n := len([]rune(name)); n > MaxNameLen {
		return fmt.Errorf("name is %d characters; the maximum is %d", n, MaxNameLen)
	}
	if strings.ContainsAny(name, "/\\") {
		return fmt.Errorf("name %q may not contain a path separator; it is a label, never a folder name", name)
	}
	if name == ".." || strings.Contains(name, "..") {
		return fmt.Errorf("name %q may not contain %q", name, "..")
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("name contains a control character (U+%04X); it must be a single line of plain text", r)
		}
	}
	return nil
}

// requireLocalhost enforces safety invariant #1 at config-parse time: a remote
// inference endpoint can never be configured, let alone dialled.
//
// 0.0.0.0 is deliberately not accepted. It is a bind address ("listen on every
// interface"), not a destination, and both Ollama call sites reject it again at
// request time — so accepting it here would let a user save a vault.yaml that
// validates cleanly and then fails at the first classification, which teaches
// them about the mistake at the worst possible moment.
func requireLocalhost(endpoint string) error {
	if endpoint == "" {
		return nil
	}
	host := endpoint
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	if i := strings.IndexAny(host, "/?#"); i >= 0 {
		host = host[:i]
	}
	if i := strings.LastIndex(host, "@"); i >= 0 {
		host = host[i+1:]
	}
	if i := strings.LastIndex(host, ":"); i >= 0 && !strings.Contains(host[i:], "]") {
		host = host[:i]
	}
	host = strings.Trim(host, "[]")
	switch strings.ToLower(host) {
	case "localhost", "127.0.0.1", "::1":
		return nil
	}
	if strings.ToLower(host) == "0.0.0.0" {
		return fmt.Errorf("%q is a bind address, not a destination; use http://localhost:11434 (or 127.0.0.1)", endpoint)
	}
	return fmt.Errorf("%q is not localhost; Kagaz never sends document text off the machine", endpoint)
}

// Person resolves a person by name or tag, case-insensitively.
func (c *Config) Person(nameOrTag string) (Person, bool) {
	want := strings.ToLower(nameOrTag)
	for _, p := range c.People {
		if strings.ToLower(p.Name) == want || strings.ToLower(p.Tag) == want {
			return p, true
		}
	}
	return Person{}, false
}

// CategoryFor returns the category definition for a category name.
func (c *Config) CategoryFor(name string) (Category, bool) {
	cat, ok := c.Structure[name]
	return cat, ok
}

// AuditLogPath is the absolute path of the append-only audit log.
func (c *Config) AuditLogPath() string {
	if filepath.IsAbs(c.Confidence.AuditLog) {
		return c.Confidence.AuditLog
	}
	return filepath.Join(c.VaultRoot, c.Confidence.AuditLog)
}

// ManifestDir is where operation manifests are written.
func (c *Config) ManifestDir() string {
	return filepath.Join(c.VaultRoot, "manifests")
}

// StagingDir is the never-delete staging area. Kagaz renames into it; the user
// empties it from Finder.
func (c *Config) StagingDir() string {
	return filepath.Join(c.VaultRoot, "_To-Delete-After-Verification")
}

// Slug lowercases a string and collapses non-alphanumerics into single dashes.
func Slug(s string) string {
	var b strings.Builder
	lastDash := true // suppress a leading dash
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
