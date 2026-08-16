package ingest

import (
	"fmt"
	"strings"
)

// Preview renders the proposal as the block the CLI shows in a numbered batch
// review. It is plain text with no colour or terminal control, so it is
// equally usable in a log, a test golden file and a pipe.
//
// The explanations are part of the preview rather than hidden behind a verbose
// flag on purpose: the whole reason inference records why it guessed is so the
// person approving the move actually sees it.
func (p Proposal) Preview() string {
	var b strings.Builder

	fmt.Fprintf(&b, "%d. %s\n", p.Index, p.Source)
	if p.Skip {
		fmt.Fprintf(&b, "   SKIP  %s\n", p.SkipReason)
		for _, w := range p.Warnings {
			fmt.Fprintf(&b, "   warn  %s\n", w)
		}
		return b.String()
	}

	fmt.Fprintf(&b, "   ->    %s\n", p.Dest)
	fmt.Fprintf(&b, "   type  %s (%s, confidence %.2f, classifier %s, ocr %s)\n",
		p.DocType, p.Category, p.Confidence, p.Classifier, p.OCREngine)

	if len(p.Owners) > 0 {
		fmt.Fprintf(&b, "   owner %s\n", strings.Join(p.Owners, ", "))
	} else {
		fmt.Fprintf(&b, "   owner (none -- shared/unowned)\n")
	}
	fmt.Fprintf(&b, "   year  %d\n", p.Year)
	fmt.Fprintf(&b, "   ident %s\n", p.Identifier)

	if len(p.Tags) > 0 {
		fmt.Fprintf(&b, "   tags  %s\n", strings.Join(p.Tags, ", "))
	}
	for _, d := range p.DroppedTags {
		fmt.Fprintf(&b, "   tag?  %s not applied: %s\n", d.Tag, d.Reason)
	}
	for _, d := range p.DroppedFields {
		fmt.Fprintf(&b, "   field %s=%q not recorded: %s\n", d.Field, d.Value, d.Reason)
	}
	for _, line := range p.Explain() {
		fmt.Fprintf(&b, "   why   %s\n", line)
	}
	for _, w := range p.Warnings {
		fmt.Fprintf(&b, "   warn  %s\n", w)
	}
	return b.String()
}

// Explain returns one sentence per inferred value, in a stable order, saying
// why it was chosen. An empty result is impossible for a non-skipped proposal:
// every guess is accounted for.
func (p Proposal) Explain() []string {
	var out []string
	if p.Why.DocType.Detail != "" {
		out = append(out, p.Why.DocType.Detail)
	}
	for _, r := range p.Why.Owners {
		if r.Detail != "" {
			out = append(out, r.Detail)
		}
	}
	if p.Why.Year.Detail != "" {
		out = append(out, p.Why.Year.Detail)
	}
	if p.Why.Identifier.Detail != "" {
		out = append(out, p.Why.Identifier.Detail)
	}
	return out
}

// PreviewBatch renders every proposal, ready to be shown above a selection
// prompt.
func PreviewBatch(proposals []Proposal) string {
	var b strings.Builder
	for _, p := range proposals {
		b.WriteString(p.Preview())
	}
	return b.String()
}

// Guessed reports whether any part of this proposal rests on a weak inference:
// a year taken from the file's modification time, an identifier that could not
// be derived at all, or an owner matched only on a given name. The CLI can use
// it to draw attention to the proposals most worth a second look.
func (p Proposal) Guessed() bool {
	if p.Why.Year.Source == SourceModTime {
		return true
	}
	if p.Why.Identifier.Source == SourceNone {
		return true
	}
	for _, r := range p.Why.Owners {
		if r.Source == SourceGivenName {
			return true
		}
	}
	return false
}
