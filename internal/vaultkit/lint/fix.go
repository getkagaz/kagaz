package lint

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/getkagaz/kagaz/internal/vaultkit/move"
	"github.com/getkagaz/kagaz/internal/vaultkit/tags"
)

// FixResult reports what `--fix` did.
type FixResult struct {
	// Fixed are the findings that were repaired.
	Fixed []Finding
	// Skipped are the findings `--fix` deliberately did not touch, either
	// because they have no provably-safe repair or because applying one failed.
	Skipped []Finding
	// Manifest is the move manifest covering every repair that relocated a
	// file. Nil when no file moved.
	Manifest *move.Manifest
	// Warnings records repairs that degraded rather than failed — a tag that
	// could not be written because the filesystem has no extended attributes,
	// say.
	Warnings []string
}

// Fix applies the provably-safe repairs among findings.
//
// Every relocation goes through move.Engine in a single batch, so one lint fix
// produces one manifest and `kagaz rollback` undoes the whole run. Findings
// with no Repair are returned in Skipped untouched.
//
// A file with several move repairs against it (a filename that is both
// mis-rendered and in the wrong folder) is moved once, to the canonical path
// both findings agree on.
func (l *Linter) Fix(findings []Finding) (*FixResult, error) {
	res := &FixResult{}
	engine, err := l.engine()
	if err != nil {
		return nil, err
	}

	// Collect one destination per source path, and the tag repairs separately.
	dests := map[string]string{}
	var moveOrder []string
	movedBy := map[string][]int{}
	tagged := map[string][]int{}

	for i, f := range findings {
		switch {
		case f.Repair == nil:
			res.Skipped = append(res.Skipped, f)
		case f.Repair.MoveTo != "":
			dst := filepath.Join(l.cfg.VaultRoot, filepath.FromSlash(f.Repair.MoveTo))
			if prev, ok := dests[f.AbsPath]; ok && prev != dst {
				// Two rules disagree about where the file belongs. That is a
				// bug in the rules, not something to resolve by picking one.
				res.Skipped = append(res.Skipped, f)
				continue
			}
			if _, ok := dests[f.AbsPath]; !ok {
				dests[f.AbsPath] = dst
				moveOrder = append(moveOrder, f.AbsPath)
			}
			movedBy[f.AbsPath] = append(movedBy[f.AbsPath], i)
		case f.Repair.AddTag != "":
			tagged[f.AbsPath] = append(tagged[f.AbsPath], i)
		default:
			res.Skipped = append(res.Skipped, f)
		}
	}

	sort.Strings(moveOrder)
	ops := make([]move.Op, 0, len(moveOrder))
	for _, src := range moveOrder {
		ops = append(ops, move.Op{Src: src, Dst: dests[src]})
	}

	// finalPath tracks where a file ends up, so a tag repair on a file that
	// also moved is applied to the destination rather than to a staged source.
	finalPath := map[string]string{}
	if len(ops) > 0 {
		out, err := engine.Execute("lint-fix", ops)
		if out != nil {
			res.Manifest = out.Manifest
			res.Warnings = append(res.Warnings, out.Warnings...)
		}
		if err != nil {
			return res, fmt.Errorf("lint --fix: %w", err)
		}
		moved := map[string]string{}
		for _, op := range out.Moved {
			moved[op.Src] = op.Dst
			finalPath[op.Src] = op.Dst
		}
		for _, src := range moveOrder {
			idxs := movedBy[src]
			if _, ok := moved[src]; ok {
				for _, i := range idxs {
					res.Fixed = append(res.Fixed, findings[i])
				}
				continue
			}
			for _, i := range idxs {
				res.Skipped = append(res.Skipped, findings[i])
			}
		}
	}

	for _, src := range sortedKeys(tagged) {
		target := src
		if p, ok := finalPath[src]; ok {
			target = p
		}
		for _, i := range tagged[src] {
			f := findings[i]
			err := tags.Add(target, f.Repair.AddTag)
			switch {
			case err == nil:
				res.Fixed = append(res.Fixed, f)
			case errors.Is(err, tags.ErrUnsupported):
				res.Warnings = append(res.Warnings, fmt.Sprintf("%s: cannot add tag %q: this filesystem does not support extended attributes", f.Path, f.Repair.AddTag))
				res.Skipped = append(res.Skipped, f)
			default:
				res.Warnings = append(res.Warnings, fmt.Sprintf("%s: cannot add tag %q: %v", f.Path, f.Repair.AddTag, err))
				res.Skipped = append(res.Skipped, f)
			}
		}
	}

	sortFindings(res.Fixed)
	sortFindings(res.Skipped)
	sort.Strings(res.Warnings)
	return res, nil
}

// engine returns the configured move engine, building the standard one for this
// vault if the caller did not supply it.
func (l *Linter) engine() (*move.Engine, error) {
	if l.Engine != nil {
		return l.Engine, nil
	}
	if l.cfg == nil {
		return nil, errNoEngine
	}
	l.Engine = move.New(l.cfg.ManifestDir(), l.cfg.StagingDir())
	return l.Engine, nil
}

func sortedKeys(m map[string][]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
