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
	"strconv"
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
	// SourceHuman means a person stated the value on the command line (the
	// --set-* flags). It is not an inference and was not checked against the
	// document; it is the one source that outranks every other because a person
	// looked at the document and said so.
	SourceHuman = "human"
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
	// "apple", "mlx:<model>", "ollama:<model>"), or ClassifierHuman when the
	// user stated the doctype with --set-doctype and no tier decided it.
	Classifier string `json:"classifier"`
	// OCREngine is the extraction backend ("pdftotext", "vision",
	// "ollama:<model>", "none").
	OCREngine string `json:"ocr_engine"`
	// Fields are the extracted structured values.
	Fields map[string]string `json:"fields,omitempty"`
	// DroppedFields are fields the classifier's model proposed and the chain
	// withheld -- because the value could not be found in the document text,
	// or because the rules tier extracted the same field from the text
	// directly. They are shown in the preview so an absent field is explained
	// rather than silent.
	DroppedFields []classify.DroppedField `json:"dropped_fields,omitempty"`
	// Degraded names a semantic tier that was asked and did not answer, so
	// this proposal came from the tier below it. Nil is the normal case.
	//
	// Without it "unclassified" reads as a finding about the document, when
	// it can mean the classifier never replied -- a timeout on a cold model,
	// a crashed helper. The two need different actions from the reader, so
	// they are not allowed to look the same.
	Degraded *classify.Degradation `json:"degraded,omitempty"`
	// Text is the extracted text. Execute truncates it into the sidecar.
	Text string `json:"-"`

	// Owners, Year and Identifier are inferred. See Why.
	Owners     []string `json:"owners,omitempty"`
	Year       int      `json:"year,omitempty"`
	Identifier string   `json:"identifier,omitempty"`

	// Dest is the proposed absolute destination path. Empty when Skip is set.
	Dest string `json:"dest,omitempty"`
	// Tags are the vocabulary-checked tags Kagaz itself contributes. They are
	// this proposal's *delta*, not the outcome -- see TagsAfter, which is what
	// the preview shows and what the filed document ends up with.
	Tags []string `json:"tags,omitempty"`
	// TagsBefore are the Finder tags already on the source file. move.CopyFile
	// carries a source's extended attributes to the destination, so these
	// arrive in the vault whether or not Kagaz proposed them.
	TagsBefore []string `json:"tags_before,omitempty"`
	// TagsAfter is the tag set the filed document will actually carry: the
	// in-vocabulary subset of TagsBefore, merged with Tags. This is the field
	// to show a user and the field to compare against reality; it mirrors the
	// {before, after} pair `kagaz tag --propose-only` returns.
	TagsAfter []string `json:"tags_after,omitempty"`
	// DroppedTags are tags that were proposed or inherited and withheld, with
	// the reason.
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
	return p.AnalyzeWith(ctx, paths, Overrides{})
}

// AnalyzeWith is Analyze with facts the user has stated rather than inferred.
//
// The overrides apply to every path in the call. Everything not overridden is
// still inferred exactly as Analyze infers it, and extraction and field
// extraction still run in full: a person naming the doctype says what the
// document is, not that its text and fields are unwanted.
//
// Invalid overrides are rejected before any file is read, so a mistyped doctype
// or an unknown owner costs a message rather than a batch of OCR.
func (p *Pipeline) AnalyzeWith(ctx context.Context, paths []string, ov Overrides) ([]Proposal, error) {
	ov, err := ov.resolve(p.Cfg, p.Catalog)
	if err != nil {
		return nil, err
	}
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
		prop, err := p.analyzeOne(ctx, path, ov)
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
//
// ov holds the already-validated facts the user stated. Each one replaces the
// corresponding inference and records that a person supplied it; nothing else
// changes, including OCR and field extraction, which run either way.
func (p *Pipeline) analyzeOne(ctx context.Context, path string, ov Overrides) (Proposal, error) {
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
		//
		// The prefix is skipped when the error already says it: a bare
		// ocr.ErrNoText rendered as "no text extracted: no text extracted",
		// which reads like two different problems.
		if msg := err.Error(); strings.Contains(msg, ocr.ErrNoText.Error()) {
			prop.Warnings = append(prop.Warnings, msg)
		} else {
			prop.Warnings = append(prop.Warnings, fmt.Sprintf("no text extracted: %v", msg))
		}
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
	prop.DroppedFields = cls.Dropped
	prop.Degraded = cls.Degraded
	prop.Why.DocType = Reason{
		Value:  cls.DocType,
		Source: SourceClassifier,
		Detail: fmt.Sprintf("classified as %q by the %s tier with confidence %.2f", cls.DocType, cls.Engine, cls.Confidence),
	}
	if cls.Degraded != nil {
		// Named before the answer it produced: the reader needs to know the
		// configured tier is not the one that spoke.
		prop.Why.DocType.Detail = fmt.Sprintf(
			"the %s tier did not answer (%s), so this came from the %s tier instead: %s",
			cls.Degraded.Engine, cls.Degraded.Reason, cls.Engine, prop.Why.DocType.Detail)
	}
	if ov.DocType != "" {
		p.applyDocTypeOverride(&prop, ov.DocType, cls)
	}

	if prop.DocType == doctypes.Unclassified || prop.Category == "" {
		prop.Skip = true
		prop.SkipReason = "the document type could not be determined, so no destination is proposed; " +
			"filing it under a guessed category would be inventing one. Say what it is with " +
			"`kagaz ingest --set-doctype <name>`, file it by hand with `kagaz move`, " +
			"or add a doctype to vault.yaml."
		if cls.Degraded != nil {
			// A different sentence, not a prefix on the one above: this is the
			// difference between "kagaz read this and could not tell" and
			// "kagaz never got an answer". Only the second is worth retrying,
			// and only the second points at the engine rather than the file,
			// so telling the user to add a doctype would be the wrong advice.
			prop.SkipReason = fmt.Sprintf(
				"the %s tier did not answer (%s), so only the %s tier read this document, and it "+
					"matched nothing. Nothing here says the document is unclassifiable -- the "+
					"engine that would have judged it never replied. Run `kagaz doctor` and try "+
					"again; if it keeps failing, `--set-doctype <name>` files it without a model.",
				cls.Degraded.Engine, cls.Degraded.Reason, cls.Engine)
		}
		prop.Why.DocType.Detail = fmt.Sprintf("no tier reached the confidence threshold; the %s tier returned %s", cls.Engine, doctypes.Unclassified)
		return prop, nil
	}

	owners, ownerWhy := inferOwners(p.Cfg, path, res.Text)
	if len(ov.Owners) > 0 {
		owners, ownerWhy = ov.Owners, statedOwners(ov.Owners)
	}
	prop.Owners, prop.Why.Owners = owners, ownerWhy

	prop.Year, prop.Why.Year = inferYear(prop.Fields, st.ModTime())
	if ov.Year > 0 {
		prop.Year, prop.Why.Year = ov.Year, Reason{
			Value:  strconv.Itoa(ov.Year),
			Source: SourceHuman,
			Detail: fmt.Sprintf("year %d: you specified it with --set-year, so no year was inferred from the document or the file's date", ov.Year),
		}
	}
	prop.Identifier, prop.Why.Identifier = inferIdentifier(prop.Fields, path, prop.DocType, owners)
	if ov.Identifier != "" {
		prop.Identifier, prop.Why.Identifier = ov.Identifier, Reason{
			Value:  ov.Identifier,
			Source: SourceHuman,
			Detail: fmt.Sprintf("identifier %q: you specified it with --set-identifier, so no identifier was inferred from the fields or the file name", ov.Identifier),
		}
	}

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
	prop.TagsBefore = p.readSourceTags(path)
	prop.TagsAfter, prop.DroppedTags = p.resultingTags(prop.TagsBefore, prop.Tags, prop.DroppedTags)
	return prop, nil
}

// applyDocTypeOverride records a doctype a person stated rather than a tier
// inferred.
//
// Three things change and no more:
//
//   - the doctype, and with it the category, which is read from the catalog and
//     never from the flags. A person may pick any real doctype; nobody, human or
//     model, may invent a category (Global Constraint 8);
//   - the classifier, to ClassifierHuman, and the confidence, to zero. A human
//     assignment is not a probability, and dressing it up as 1.00 would make it
//     indistinguishable, in a sort or a filter, from a model that was very sure.
//     The sidecar omits a zero confidence entirely, which is the honest record:
//     `classifier: human` says who decided, and nothing pretends to have scored
//     it;
//   - the fields, which are re-extracted with the stated doctype's own patterns.
//     The classifier's fields are kept where the stated doctype has nothing to
//     say, so overriding never costs the user data that was already extracted.
//
// What the tier had answered is kept in the rationale rather than discarded: an
// override over a confident "invoice" is a different event from an override over
// "unclassified", and the record should be able to tell them apart.
func (p *Pipeline) applyDocTypeOverride(prop *Proposal, name string, cls classify.Result) {
	category, _ := p.Catalog.CategoryOf(name)
	prop.DocType = name
	prop.Category = category
	prop.Classifier = ClassifierHuman
	prop.Confidence = 0
	prop.Fields = mergeFields(p.Catalog.ExtractFields(name, prop.Text), cls.Fields)

	had := cls.DocType
	if had == "" {
		had = doctypes.Unclassified
	}
	prop.Why.DocType = Reason{
		Value:  name,
		Source: SourceHuman,
		Detail: fmt.Sprintf("doctype %q: you specified it with --set-doctype, so it was not inferred (the %s tier had answered %q); "+
			"the category %q comes from the vault's doctype catalog, not from the flag", name, cls.Engine, had, category),
	}
}

// mergeFields combines the stated doctype's own extractions with the ones the
// classifier produced. Deterministic values for the doctype the user chose win;
// anything the classifier found that the chosen doctype does not extract is
// kept rather than thrown away.
func mergeFields(primary, extra map[string]string) map[string]string {
	if len(primary) == 0 && len(extra) == 0 {
		return nil
	}
	out := make(map[string]string, len(primary)+len(extra))
	for k, v := range extra {
		out[k] = v
	}
	for k, v := range primary {
		out[k] = v
	}
	return out
}

// statedOwners builds the rationale for owners the user named.
func statedOwners(owners []string) []Reason {
	out := make([]Reason, 0, len(owners))
	for _, name := range owners {
		out = append(out, Reason{
			Value:  name,
			Source: SourceHuman,
			Detail: fmt.Sprintf("owner %s: you specified it with --set-owner, so no owner inference ran against the file name or the document text", name),
		})
	}
	return out
}

// readSourceTags reads the Finder tags already on a source file.
//
// They matter because a move copies them: move.CopyFile carries the source's
// extended attributes to the destination, so an ingested document arrives
// wearing whatever the user (or another tool) had tagged it with, before Kagaz
// adds anything. A filesystem without xattr support simply has none.
func (p *Pipeline) readSourceTags(path string) []string {
	existing, err := tags.Read(path)
	if err != nil {
		return nil
	}
	return tags.Normalize(existing)
}

// resultingTags computes the tag set a filed document will really carry, and
// explains every inherited tag that did not make it.
//
// Inherited tags outside the vault's controlled vocabulary are DROPPED rather
// than carried. Three reasons, in order of weight:
//
//   - The preview must be able to promise something. An out-of-vocabulary tag
//     carried into the vault makes the freshly ingested document fail the
//     vault's own `kagaz lint` immediately, which is a defect the user did not
//     ask for and cannot see coming.
//   - It is the same rule ingest already applies to the tags it proposes
//     itself (proposeTags). A tag being inherited rather than inferred is not
//     a reason to hold it to a weaker standard.
//   - Nothing is lost. A move retires the source into the vault's staging
//     folder with its extended attributes intact, so the original tagging
//     survives until the user empties staging.
//
// In-vocabulary inherited tags ARE carried, and are shown. That deliberately
// includes "confidential", which changes whether `resolve --for-send` gates
// the document: silently dropping it would weaken a document's security
// posture just as surely as silently adding it would misstate it. The rule is
// that the user sees the true outcome and approves it -- not that Kagaz picks
// the safer-looking answer on their behalf.
func (p *Pipeline) resultingTags(before, added []string, dropped []DroppedTag) ([]string, []DroppedTag) {
	seen := map[string]bool{}
	after := make([]string, 0, len(before)+len(added))
	for _, tag := range before {
		if seen[tag] {
			continue
		}
		seen[tag] = true
		if p.Vocab != nil && !p.Vocab.Known(tag) {
			dropped = append(dropped, DroppedTag{
				Tag: tag,
				Reason: "already on the source file, but it is not in the vault's tag vocabulary; " +
					"it is not carried into the vault (the source keeps it in staging). Add it to vault.yaml to keep it",
			})
			continue
		}
		after = append(after, tag)
	}
	for _, tag := range added {
		if seen[tag] {
			continue
		}
		seen[tag] = true
		after = append(after, tag)
	}
	sort.Strings(after)
	return after, dropped
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
