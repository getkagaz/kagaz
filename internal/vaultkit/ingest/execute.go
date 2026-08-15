package ingest

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"

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
