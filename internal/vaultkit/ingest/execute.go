package ingest

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/getkagaz/kagaz/internal/vaultkit/audit"
	"github.com/getkagaz/kagaz/internal/vaultkit/move"
	"github.com/getkagaz/kagaz/internal/vaultkit/sidecar"
	"github.com/getkagaz/kagaz/internal/vaultkit/tags"
)

// Result reports what Execute did.
type Result struct {
	// Move is the move engine's result, including the manifest that
	// `kagaz rollback` reverses.
	Move *move.Result
	// Filed lists the proposals that were moved, with Dest as executed.
	Filed []Proposal
	// Skipped lists proposals that were not acted on, including any the
	// analysis had already marked Skip.
	Skipped []Proposal
	// Sidecars lists the sidecar files written.
	Sidecars []string
	// Warnings are non-fatal problems: a filesystem without extended
	// attributes, an unwritable sidecar, a tag that could not be set.
	Warnings []string
}

// ManifestPath is the manifest for this batch, or "" when nothing was written.
func (r *Result) ManifestPath() string {
	if r == nil || r.Move == nil || r.Move.Manifest == nil {
		return ""
	}
	return r.Move.Manifest.Path
}

// Execute performs the approved proposals: one manifest for the whole batch,
// every file moved through move.Engine, then the sidecar and the tags at the
// destination, then one audit line.
//
// The order matters and is not arbitrary:
//
//   - move.Engine writes the manifest before the first byte moves, so an
//     interruption anywhere after that leaves a manifest `kagaz rollback` can
//     reverse -- including a batch that failed half way, whose moved files are
//     all recorded;
//   - the sidecar is written at the *destination*, after the move, because
//     move.Engine carries a sidecar back with its document on rollback, so a
//     reversed ingest takes its sidecar with it instead of orphaning one;
//   - a sidecar or tag problem is a warning, never a failure. The document is
//     already safely filed at that point, and failing the batch over an xattr
//     the filesystem does not support would be a worse outcome than a document
//     with no Finder tag.
//
// Proposals marked Skip are returned untouched in Result.Skipped.
func (p *Pipeline) Execute(proposals []Proposal) (*Result, error) {
	res := &Result{}

	ops := make([]move.Op, 0, len(proposals))
	acting := make([]Proposal, 0, len(proposals))
	for _, prop := range proposals {
		if prop.Skip || prop.Dest == "" {
			res.Skipped = append(res.Skipped, prop)
			continue
		}
		if prop.Source == prop.Dest {
			res.Skipped = append(res.Skipped, prop)
			continue
		}
		// Proposals arrive from a CLI that lets a user edit them, so the
		// destination is re-checked here rather than trusted from Analyze. This
		// runs before the manifest is written, so a rejected batch has done
		// nothing at all.
		if err := p.checkDest(prop.Dest); err != nil {
			return res, fmt.Errorf("ingest: %s: %w", filepath.Base(prop.Source), err)
		}
		prop.Tags = p.recheckTags(&prop)
		ops = append(ops, move.Op{Src: prop.Source, Dst: prop.Dest})
		acting = append(acting, prop)
	}
	if len(ops) == 0 {
		res.Move = &move.Result{}
		return res, nil
	}
	if p.Engine == nil {
		return res, errors.New("ingest: no move engine configured")
	}

	moved, moveErr := p.Engine.Execute(OpName, ops)
	res.Move = moved
	if moved != nil {
		res.Warnings = append(res.Warnings, moved.Warnings...)
	}

	// Even a partially failed batch has really moved some files. Their
	// sidecars and tags are written so the vault is not left with filed
	// documents that Kagaz knows nothing about.
	byDest := map[string]bool{}
	if moved != nil {
		for _, op := range moved.Moved {
			byDest[op.Dst] = true
		}
	}
	for _, prop := range acting {
		dest := actualDest(moved, prop)
		if dest == "" || !byDest[dest] {
			res.Skipped = append(res.Skipped, prop)
			continue
		}
		prop.Dest = dest
		res.Filed = append(res.Filed, prop)

		if err := p.writeSidecar(prop); err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("%s: sidecar not written: %v", filepath.Base(dest), err))
		} else {
			res.Sidecars = append(res.Sidecars, sidecar.Path(dest))
		}
		if err := p.applyTags(prop); err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("%s: tags not applied: %v", filepath.Base(dest), err))
		}
	}

	if err := p.appendAudit(res); err != nil {
		res.Warnings = append(res.Warnings, fmt.Sprintf("audit line not written: %v", err))
	}
	return res, moveErr
}

// checkDest refuses a destination that is not a plain file inside the vault.
//
// Analyze builds destinations from the vault's own conventions and could never
// produce one of these. Execute is the mutating half of a public API that the
// CLI feeds user-edited proposals into, so it verifies rather than assumes: an
// unchecked Dest is an arbitrary-file-write through a tool whose entire promise
// is that it only ever moves documents inside your vault.
func (p *Pipeline) checkDest(dest string) error {
	if p.Cfg == nil || p.Cfg.VaultRoot == "" {
		return errors.New("no vault root configured")
	}
	if !filepath.IsAbs(dest) {
		return fmt.Errorf("destination %q is not an absolute path", dest)
	}
	clean := filepath.Clean(dest)

	root := filepath.Clean(p.Cfg.VaultRoot)
	rel, err := filepath.Rel(root, clean)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("destination %q is outside the vault root %q", dest, root)
	}
	// Kagaz's own bookkeeping areas are not filing destinations. Writing a
	// document into staging would queue it for the user to delete; writing one
	// into manifests would corrupt the rollback record.
	for _, reserved := range []string{p.Cfg.StagingDir(), p.Cfg.ManifestDir()} {
		if r, err := filepath.Rel(filepath.Clean(reserved), clean); err == nil &&
			r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator)) {
			return fmt.Errorf("destination %q is inside %q, which Kagaz reserves for its own bookkeeping", dest, reserved)
		}
	}
	if sidecar.IsSidecar(clean) {
		return fmt.Errorf("destination %q is a sidecar path; sidecars travel with their document", dest)
	}
	if st, err := os.Stat(clean); err == nil && st.IsDir() {
		return fmt.Errorf("destination %q is an existing directory", dest)
	}
	return nil
}

// recheckTags re-runs the controlled-vocabulary check on a proposal's tags at
// execution time.
//
// Analyze already filtered them, but a caller may have edited the proposal in
// between -- the CLI is about to let a user do exactly that -- and the
// vocabulary check is the whole reason Finder tags stay searchable. Rejected
// tags are moved to DroppedTags rather than silently discarded.
func (p *Pipeline) recheckTags(prop *Proposal) []string {
	if p.Vocab == nil || len(prop.Tags) == 0 {
		return prop.Tags
	}
	kept := make([]string, 0, len(prop.Tags))
	for _, tag := range tags.Normalize(prop.Tags) {
		if p.Vocab.Known(tag) {
			kept = append(kept, tag)
			continue
		}
		prop.DroppedTags = append(prop.DroppedTags, DroppedTag{
			Tag:    tag,
			Reason: "not in the vault's tag vocabulary at execution time; add it to vault.yaml to use it",
		})
	}
	return kept
}

// actualDest resolves the destination the engine really used, which differs
// from the proposal when a collision policy suffixed the name.
func actualDest(moved *move.Result, prop Proposal) string {
	if moved == nil {
		return ""
	}
	for _, op := range moved.Moved {
		if op.Src == prop.Source {
			return op.Dst
		}
	}
	return ""
}

// writeSidecar records everything ingest learned, next to the filed document.
func (p *Pipeline) writeSidecar(prop Proposal) error {
	meta := &sidecar.Meta{
		ExtractedAt: p.now().Format("2006-01-02"),
		OCREngine:   prop.OCREngine,
		Classifier:  prop.Classifier,
		DocType:     prop.DocType,
		Category:    prop.Category,
		Confidence:  prop.Confidence,
		Owners:      prop.Owners,
		Identifier:  prop.Identifier,
		Year:        prop.Year,
		Fields:      prop.Fields,
		SourceSHA:   prop.SourceSHA,
		Text:        prop.Text,
	}
	return sidecar.Write(prop.Dest, meta)
}

// applyTags sets the proposal's (already vocabulary-checked) tags on the filed
// document. A filesystem without extended attributes is reported as a warning
// by the caller, never as a failure: Linux CI and many network mounts are in
// that state and the document is filed correctly regardless.
func (p *Pipeline) applyTags(prop Proposal) error {
	if len(prop.Tags) == 0 {
		return nil
	}
	if err := tags.Add(prop.Dest, prop.Tags...); err != nil {
		if errors.Is(err, tags.ErrUnsupported) {
			return fmt.Errorf("%w (Finder tags are unavailable on this filesystem)", err)
		}
		return err
	}
	return nil
}

// appendAudit writes the single audit line for the batch. It records paths and
// counts only: no document text, no extracted field values, nothing that could
// carry a secret into a log (Global Constraint 6).
func (p *Pipeline) appendAudit(res *Result) error {
	if p.Audit == nil || len(res.Filed) == 0 {
		return nil
	}
	paths := make([]string, 0, len(res.Filed))
	for _, prop := range res.Filed {
		paths = append(paths, prop.Dest)
	}
	return p.Audit.Append(audit.Entry{
		Op:       OpName,
		Paths:    paths,
		Manifest: res.ManifestPath(),
		Detail: map[string]string{
			"documents": strconv.Itoa(len(res.Filed)),
			"skipped":   strconv.Itoa(len(res.Skipped)),
		},
	})
}
