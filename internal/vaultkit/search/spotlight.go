package search

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
	"time"
)

// Finder narrows a query to a set of candidate paths.
//
// A Finder is an accelerator and never a source of truth, and the way that is
// guaranteed is that its answer cannot reach the result set at all: Kagaz reads
// the sidecars of the candidates early (see Searcher.prefetch) and then walks
// the whole vault regardless, deciding every match from the filesystem. A
// Spotlight index that is stale, partial, over-eager or absent therefore costs
// at most some wasted I/O.
//
// `kagaz find` returns identical results with and without a Finder — not
// "identical on a current index", identical. The equivalence test in this
// package asserts that on the fixture vault across complete, under-reporting,
// over-reporting, empty, declining and failing Finders, and passing a nil
// Spotlight is all it takes to force the pure filesystem path.
type Finder interface {
	// Available reports whether the accelerator can be used at all.
	Available() bool
	// Narrow returns candidate absolute paths under root. ok=false means
	// Spotlight could not answer this query and the caller must consider every
	// file, which is also what a nil error with an empty result means.
	Narrow(ctx context.Context, root string, q Query) (paths []string, ok bool, err error)
}

// MDFindTimeout caps how long the accelerator may take. Spotlight is an
// optimisation; a slow answer is worse than no answer, because the walk that
// follows is the authoritative pass either way.
const MDFindTimeout = 10 * time.Second

// MDFind is the Spotlight accelerator, shelling out to macOS's mdfind. On any
// system without mdfind — Linux CI, a Mac with Spotlight disabled — it reports
// itself unavailable and Kagaz falls back to the plain filesystem walk.
type MDFind struct {
	// Bin overrides the mdfind binary. Empty means look it up on $PATH.
	Bin string
}

// NewMDFind returns a Spotlight accelerator using mdfind from $PATH.
func NewMDFind() *MDFind { return &MDFind{} }

// Available reports whether mdfind is installed.
func (m *MDFind) Available() bool {
	_, err := m.binary()
	return err == nil
}

func (m *MDFind) binary() (string, error) {
	if m.Bin != "" {
		return m.Bin, nil
	}
	return exec.LookPath("mdfind")
}

// ErrNoMDFind means Spotlight's command-line client is not installed.
var ErrNoMDFind = errors.New("mdfind not found")

// Narrow runs `mdfind -onlyin <root> <expr>` for the query's content terms.
func (m *MDFind) Narrow(ctx context.Context, root string, q Query) ([]string, bool, error) {
	bin, err := m.binary()
	if err != nil {
		return nil, false, ErrNoMDFind
	}
	expr := MDFindExpr(q)
	if expr == "" {
		return nil, false, nil
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, MDFindTimeout)
		defer cancel()
	}

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, "-onlyin", root, expr)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, false, err
	}
	var paths []string
	for _, line := range strings.Split(stdout.String(), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			paths = append(paths, line)
		}
	}
	return paths, true, nil
}

// MDFindExpr builds the Spotlight metadata query for a Query's content terms,
// or "" when the query has none. It is exported because `kagaz index` pastes
// the same expressions into a vault's INDEX.md and AGENTS.md, and the two must
// not drift.
func MDFindExpr(q Query) string {
	var terms []string
	for _, t := range append(tagTerms(q), textTerm(q)) {
		if t != "" {
			terms = append(terms, t)
		}
	}
	return strings.Join(terms, " && ")
}

func tagTerms(q Query) []string {
	var out []string
	seen := map[string]bool{}
	list := append([]string(nil), q.Tags...)
	if q.Active {
		list = append(list, "active")
	}
	for _, t := range list {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, TagQuery(t))
	}
	return out
}

func textTerm(q Query) string {
	text := mdEscape(q.Text)
	if text == "" {
		return ""
	}
	return `(kMDItemTextContent == "*` + text + `*"cd || kMDItemDisplayName == "*` + text + `*"cd)`
}

// TagQuery is the Spotlight expression matching one Finder tag.
func TagQuery(tag string) string {
	return `kMDItemUserTags == "` + mdEscape(tag) + `"c`
}

// mdEscape removes the characters that would terminate or restructure an
// mdfind expression. Nothing here is passed through a shell — exec.Command
// takes an argument vector — so this guards the query language, not the shell.
func mdEscape(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '"', '\\', '\'', '\n', '\r', '\t':
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
