// Package ingest turns a loose file into a filed document: OCR, classify,
// extract, then *propose*.
//
// # Propose, never mutate
//
// Analyze has no side effects on the vault. It reads files, runs extraction
// and classification, and returns proposals. It moves nothing, writes no
// sidecar, sets no tag and creates no folder. Every mutation happens in
// Execute, through move.Engine, under one manifest for the whole batch, with
// one audit line -- which is what makes `kagaz rollback` able to reverse an
// ingest, including one that was interrupted half way through.
//
// That split is the whole design (Global Constraint 4). A pipeline that files
// a document because it was confident is not a filing assistant, it is a thing
// that moves your documents while you are looking away.
//
// # Inference is a guess, and says so
//
// Owner, year and identifier are inferred, and inference is sometimes wrong.
// Every inferred value therefore carries a Reason recording *why* it was
// chosen -- which person's name matched and where, whether the year came from
// an extracted date or from the file's modification time, which extracted field
// supplied the identifier -- so the CLI can show it and the user can correct it
// before approving. A confident wrong guess the user cannot see is the failure
// mode this package is designed against.
package ingest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/getkagaz/kagaz/internal/vaultkit/audit"
	"github.com/getkagaz/kagaz/internal/vaultkit/classify"
	"github.com/getkagaz/kagaz/internal/vaultkit/config"
	"github.com/getkagaz/kagaz/internal/vaultkit/conventions"
	"github.com/getkagaz/kagaz/internal/vaultkit/doctypes"
	"github.com/getkagaz/kagaz/internal/vaultkit/fycal"
	"github.com/getkagaz/kagaz/internal/vaultkit/move"
	"github.com/getkagaz/kagaz/internal/vaultkit/ocr"
	"github.com/getkagaz/kagaz/internal/vaultkit/sidecar"
	"github.com/getkagaz/kagaz/internal/vaultkit/tags"
)

// OpName is the operation name stamped on the manifest and the audit line.
const OpName = "ingest"

// TextExtractor is the OCR tier. *ocr.Extractor satisfies it; tests supply
// recorded fixtures instead, which is how this package's tests run on Linux
// with no pdftotext, no Vision and no Ollama.
type TextExtractor interface {
	Extract(ctx context.Context, path string) (ocr.Result, error)
}

// Classifier is the classification tier. *classify.Chain satisfies it.
type Classifier interface {
	Classify(ctx context.Context, req classify.Request) (classify.Result, error)
}

// Reason records why an inferred value was chosen.
//
// Source is a stable machine-readable token (see the Source* constants) for
// `--json` consumers; Detail is the sentence shown to a person.
type Reason struct {
	Value  string `json:"value"`
	Source string `json:"source"`
	Detail string `json:"detail"`
}

// Reason sources. These are contract: the CLI and the MCP server may switch on
// them, so they change only with a version bump.
const (
	// SourceClassifier means a classifier tier decided it.
	SourceClassifier = "classifier"
	// SourceFilename means the source file's name supplied it.
	SourceFilename = "filename"
	// SourceText means the extracted document text supplied it.
	SourceText = "document-text"
	// SourceTag means a configured person's tag matched.
	SourceTag = "person-tag"
	// SourceGivenName means a person's given name matched and is unique among
	// the vault's people. The weakest owner match, and flagged as such.
	SourceGivenName = "given-name"
	// SourceField means an extracted structured field supplied it. Detail
	// names the field.
	SourceField = "extracted-field"
	// SourceModTime means the file's modification time supplied it -- a guess,
	// not a fact about the document.
	SourceModTime = "file-mtime"
	// SourceNone means nothing supplied it.
	SourceNone = "none"
)

// DroppedTag is a tag that was proposed and then withheld because it is
// outside the vault's controlled vocabulary.
type DroppedTag struct {
	Tag    string `json:"tag"`
	Reason string `json:"reason"`
}

// Proposal is one document's proposed filing. Nothing in it has happened yet.
type Proposal struct {
	// Index is the 1-based position in the batch, matching the number the CLI
	// shows and the number ParseSelection accepts.
	Index int `json:"index"`

	// Source is the file as it is now.
	Source string `json:"source"`
	// SourceSHA is the source's SHA256 at analysis time.
	SourceSHA string `json:"source_sha256"`
	// ModTime is the source's modification time.
	ModTime time.Time `json:"mod_time"`
	// Size is the source's size in bytes.
	Size int64 `json:"size"`

	// DocType, Category and Confidence come from the classifier chain, already
	// validated against the resolved catalog.
	DocType    string  `json:"doctype"`
	Category   string  `json:"category"`
	Confidence float64 `json:"confidence"`
	// Classifier is the backend that produced the answer ("rules",
	// "apple", "mlx:<model>", "ollama:<model>").
	Classifier string `json:"classifier"`
	// OCREngine is the extraction backend ("pdftotext", "vision",
	// "ollama:<model>", "none").
	OCREngine string `json:"ocr_engine"`
	// Fields are the extracted structured values.
	Fields map[string]string `json:"fields,omitempty"`
	// Text is the extracted text. Execute truncates it into the sidecar.
	Text string `json:"-"`

	// Owners, Year and Identifier are inferred. See Why.
	Owners     []string `json:"owners,omitempty"`
	Year       int      `json:"year,omitempty"`
	Identifier string   `json:"identifier,omitempty"`

	// Dest is the proposed absolute destination path. Empty when Skip is set.
	Dest string `json:"dest,omitempty"`
	// Tags are the vocabulary-checked tags Execute will apply.
	Tags []string `json:"tags,omitempty"`
	// DroppedTags are tags that were proposed and withheld, with the reason.
	DroppedTags []DroppedTag `json:"dropped_tags,omitempty"`

	// Why explains every inferred value.
	Why Rationale `json:"why"`

	// Skip is set when nothing should be done with this file. A skipped
	// proposal is still returned, so the CLI can show it and say why.
	Skip bool `json:"skip"`
	// SkipReason explains Skip in one sentence.
	SkipReason string `json:"skip_reason,omitempty"`
	// Warnings are non-fatal problems found while analysing.
	Warnings []string `json:"warnings,omitempty"`
}

// Rationale is the explanation for every inferred value on a Proposal.
type Rationale struct {
	DocType    Reason   `json:"doctype"`
	Owners     []Reason `json:"owners,omitempty"`
	Year       Reason   `json:"year"`
	Identifier Reason   `json:"identifier"`
}

// Pipeline runs ingest for one vault.
type Pipeline struct {
	// Cfg is the vault configuration.
	Cfg *config.Config
	// Catalog is the resolved doctype catalog.
	Catalog *doctypes.Catalog
	// Names renders conventional paths.
	Names *conventions.Conventions
	// Vocab is the controlled tag vocabulary every proposed tag is checked
	// against.
	Vocab *tags.Vocabulary
	// Extractor is the OCR tier.
	Extractor TextExtractor
	// Classifier is the classification tier.
	Classifier Classifier
	// Engine performs the moves. It is the only way this package touches the
	// filesystem outside sidecars and tags.
	Engine *move.Engine
	// Audit receives one line per executed batch.
	Audit *audit.Log
	// Now supplies timestamps; tests substitute a fixed clock.
	Now func() time.Time
}

// New builds the real pipeline for a vault: the OCR extractor, the classifier
// chain, the move engine and the audit log, all from cfg.
func New(cfg *config.Config) (*Pipeline, error) {
	if cfg == nil {
		return nil, fmt.Errorf("ingest: no vault configuration")
	}
	cat, err := doctypes.Resolve(cfg)
	if err != nil {
		return nil, err
	}
	names, err := conventions.New(cfg)
	if err != nil {
		return nil, err
	}
	return &Pipeline{
		Cfg:        cfg,
		Catalog:    cat,
		Names:      names,
		Vocab:      tags.NewVocabulary(cfg),
		Extractor:  ocr.NewExtractor(cfg, ""),
		Classifier: classify.New(cfg, cat),
		Engine:     move.New(cfg.ManifestDir(), cfg.StagingDir()),
		Audit:      audit.Open(cfg.AuditLogPath()),
		Now:        time.Now,
	}, nil
}

func (p *Pipeline) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// Analyze produces a proposal for every file in paths, in a stable order.
//
// It writes nothing. A directory is walked; sidecars, dotfiles and Kagaz's own
// manifest and staging areas are skipped. A file that cannot be read, cannot
// be extracted, or cannot be classified with confidence yields a skipped
// proposal carrying the reason, never an aborted batch -- one unreadable scan
// must not cost the user the other forty proposals.
//
// The one error it returns is a configuration problem that applies to every
// file alike, such as a forced classifier engine that is not installed.
func (p *Pipeline) Analyze(ctx context.Context, paths []string) ([]Proposal, error) {
	files, err := p.collect(paths)
	if err != nil {
		return nil, err
	}

	out := make([]Proposal, 0, len(files))
	for _, path := range files {
		// A cancelled batch must say it was cancelled. Without this check the
		// remaining files each fail extraction with a wrapped context error and
		// come back as N skipped proposals reading "no text extracted: context
		// canceled", which looks like N unreadable documents.
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("ingest: %w", err)
		}
		prop, err := p.analyzeOne(ctx, path)
		if err != nil {
			// Only a whole-vault problem reaches here.
			return nil, err
		}
		prop.Index = len(out) + 1
		out = append(out, prop)
	}
	return out, nil
}

// collect expands the requested paths into an ordered, de-duplicated file list.
func (p *Pipeline) collect(paths []string) ([]string, error) {
	seen := map[string]bool{}
	var out []string

	add := func(path string) {
		abs, err := filepath.Abs(path)
		if err != nil || seen[abs] {
			return
		}
		seen[abs] = true
		out = append(out, abs)
	}

	for _, raw := range paths {
		path := config.ExpandHome(raw)
		st, err := os.Stat(path)
		if err != nil {
			// A path the user named and that does not exist is a typo worth
			// stopping for, not a per-file skip.
			return nil, fmt.Errorf("ingest: %w", err)
		}
		if !st.IsDir() {
			add(path)
			continue
		}
		err = filepath.WalkDir(path, func(p2 string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			name := filepath.Base(p2)
			if d.IsDir() {
				if p2 != path && skipDir(name) {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasPrefix(name, ".") || sidecar.IsSidecar(p2) {
				return nil
			}
			add(p2)
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("ingest: %w", err)
		}
	}
	sort.Strings(out)
	return out, nil
}

// skipDir names the directories a walk must never descend into: Kagaz's own
// bookkeeping, and the staging area, which holds superseded copies of
// documents that are already filed.
func skipDir(name string) bool {
	switch name {
	case "manifests", "_To-Delete-After-Verification":
		return true
	}
	return strings.HasPrefix(name, ".")
}

// analyzeOne builds one proposal. It reads; it never writes.
func (p *Pipeline) analyzeOne(ctx context.Context, path string) (Proposal, error) {
	prop := Proposal{Source: path}

	st, err := os.Stat(path)
	if err != nil {
		prop.Skip, prop.SkipReason = true, fmt.Sprintf("cannot read the file: %v", err)
		return prop, nil
	}
	prop.ModTime, prop.Size = st.ModTime(), st.Size()

	sum, err := move.SHA256(path)
	if err != nil {
		prop.Skip, prop.SkipReason = true, fmt.Sprintf("cannot hash the file: %v", err)
		return prop, nil
	}
	prop.SourceSHA = sum

	res, err := p.Extractor.Extract(ctx, path)
	prop.OCREngine = res.Engine
	if prop.OCREngine == "" {
		prop.OCREngine = "none"
	}
	if err != nil {
		// No text is not fatal: the classifier still sees the filename-derived
		// nothing and answers unclassified, and the user gets a proposal that
		// says exactly why it could not be filed.
		prop.Warnings = append(prop.Warnings, fmt.Sprintf("no text extracted: %v", err))
	}
	prop.Text = res.Text

	cls, err := p.Classifier.Classify(ctx, classify.Request{Text: res.Text, Path: path, Catalog: p.Catalog})
	if err != nil {
		// classify.Chain errors only when the user forced an engine that is
		// not installed -- a vault-wide problem, not this file's.
		return Proposal{}, err
	}
	prop.DocType = cls.DocType
	prop.Category = cls.Category
	prop.Confidence = cls.Confidence
	prop.Classifier = cls.Engine
	prop.Fields = cls.Fields
	prop.Why.DocType = Reason{
		Value:  cls.DocType,
		Source: SourceClassifier,
		Detail: fmt.Sprintf("classified as %q by the %s tier with confidence %.2f", cls.DocType, cls.Engine, cls.Confidence),
	}

	if cls.DocType == doctypes.Unclassified || cls.Category == "" {
		prop.Skip = true
		prop.SkipReason = "the document type could not be determined, so no destination is proposed; " +
			"filing it under a guessed category would be inventing one. Classify it yourself with `kagaz move`, " +
			"or add a doctype to vault.yaml."
		prop.Why.DocType.Detail = fmt.Sprintf("no tier reached the confidence threshold; the %s tier returned %s", cls.Engine, doctypes.Unclassified)
		return prop, nil
	}

	owners, ownerWhy := inferOwners(p.Cfg, path, res.Text)
	prop.Owners, prop.Why.Owners = owners, ownerWhy

	prop.Year, prop.Why.Year = inferYear(cls.Fields, st.ModTime())
	prop.Identifier, prop.Why.Identifier = inferIdentifier(cls.Fields, path, cls.DocType, owners)

	doc := conventions.Doc{
		DocType:    prop.DocType,
		Category:   prop.Category,
		Owners:     prop.Owners,
		Identifier: prop.Identifier,
		Year:       prop.Year,
		Ext:        filepath.Ext(path),
	}
	dest, why, err := p.destination(doc)
	if err != nil {
		prop.Skip = true
		prop.SkipReason = fmt.Sprintf("no conventional path could be built: %v", err)
		return prop, nil
	}
	if why != "" {
		prop.Why.Owners = append(prop.Why.Owners, Reason{Source: SourceNone, Detail: why})
	}
	prop.Dest = dest
	if dest == path {
		prop.Skip = true
		prop.SkipReason = "already filed at its conventional path"
	}

	prop.Tags, prop.DroppedTags = p.proposeTags(prop)
	return prop, nil
}

// destination builds the proposed path for doc, and explains the one case a
// user would otherwise be surprised by.
//
// An unowned document under a filename pattern where {Names} is required takes
// the category's shared label in its *name* as well as its folder.
// conventions.Render owns that substitution, so the name ingest proposes is
// exactly the name lint and search read back as unowned -- ingest keeps no
// marker of its own to drift from the grammar. All that is left here is to say
// what happened, rather than quietly labelling somebody's insurance policy as
// shared. A category with no shared label makes Render fail loudly instead,
// and the caller turns that into a skip with the message Render wrote.
func (p *Pipeline) destination(doc conventions.Doc) (string, string, error) {
	dest, err := p.Names.Path(doc)
	if err != nil {
		return "", "", err
	}
	if len(doc.Owners) > 0 {
		return dest, "", nil
	}
	cat, ok := p.Cfg.CategoryFor(doc.Category)
	if !ok || cat.Shared == "" {
		return dest, "", nil
	}
	label := p.Names.Word(cat.Shared)
	base := strings.TrimSuffix(filepath.Base(dest), filepath.Ext(dest))
	for _, field := range strings.Split(base, p.Cfg.Filename.FieldSep) {
		if field != label {
			continue
		}
		return dest, fmt.Sprintf("no owner matched, and this vault's filename pattern requires a name, so the file name uses %q; "+
			"the folder is still the category's shared/unowned location. Set an owner before approving if that is wrong.", label), nil
	}
	return dest, "", nil
}

// proposeTags builds the tag set for a proposal and runs it through the
// controlled vocabulary. A tag outside the vocabulary is *not* applied: an
// uncontrolled vocabulary makes saved searches unreliable, so ingest reports
// the omission rather than quietly widening it.
func (p *Pipeline) proposeTags(prop Proposal) ([]string, []DroppedTag) {
	type candidate struct{ tag, why string }
	var want []candidate

	for _, name := range prop.Owners {
		if person, ok := p.Cfg.Person(name); ok && person.Tag != "" {
			want = append(want, candidate{person.Tag, "owner " + person.Name})
		}
	}
	if prop.Year > 0 {
		cal := fycal.New(p.Cfg.FiscalYear.StartMonth, p.Cfg.FiscalYear.LabelFormat)
		want = append(want, candidate{cal.YearStarting(prop.Year).Tag(), "fiscal year of the document"})
	}
	if slug := config.Slug(prop.Identifier); slug != "" {
		want = append(want, candidate{slug, "identifier " + prop.Identifier})
	}
	want = append(want, candidate{"active", "newly ingested documents start as active"})

	var applied []string
	var dropped []DroppedTag
	seen := map[string]bool{}
	for _, c := range want {
		tag := strings.ToLower(strings.TrimSpace(c.tag))
		if tag == "" || seen[tag] {
			continue
		}
		seen[tag] = true
		if !p.Vocab.Known(tag) {
			dropped = append(dropped, DroppedTag{
				Tag:    tag,
				Reason: fmt.Sprintf("proposed from %s, but it is not in the vault's tag vocabulary; add it to vault.yaml to use it", c.why),
			})
			continue
		}
		applied = append(applied, tag)
	}
	sort.Strings(applied)
	return applied, dropped
}
