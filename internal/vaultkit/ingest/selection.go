package ingest

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// SelectAll and SelectNone are the two whole-batch answers accepted by
// ParseSelection.
const (
	SelectAll  = "all"
	SelectNone = "none"
)

// ParseSelection turns a batch-review answer into the 1-based proposal indices
// it names, for a batch of n proposals.
//
// Accepted forms:
//
//	all      every proposal
//	none     no proposals
//	1        one proposal
//	1,3-5    a comma-separated list of single indices and inclusive ranges
//
// Whitespace around the whole answer, around each item and around a range's
// dash is ignored; the answer is case-insensitive. The result is sorted
// ascending and contains no duplicates.
//
// Everything else is an error, deliberately and without a "best effort"
// interpretation. This function is the only thing between a person typing at a
// prompt and a bulk file move, so an answer that is not unambiguously a
// selection has to be re-asked, not guessed at:
//
//   - an empty answer -- the caller decides what a bare Enter means, because
//     defaulting it here would put that decision somewhere nobody looks;
//   - an index of 0, a negative index, or one above n;
//   - a reversed range (5-3), which most plausibly means the user mistyped one
//     of the two numbers, and either repair would be a guess;
//   - an index named twice, directly or through overlapping ranges;
//   - all/none mixed with anything else;
//   - anything non-numeric, a bare "-", "1-", "-3", "1--3", "+1", "1.5".
func ParseSelection(input string, n int) ([]int, error) {
	if n < 0 {
		return nil, fmt.Errorf("selection: batch size %d is negative", n)
	}
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return nil, fmt.Errorf("selection: empty; answer %s, %s, or a list like 1,3-5", SelectAll, SelectNone)
	}

	switch strings.ToLower(trimmed) {
	case SelectAll:
		out := make([]int, 0, n)
		for i := 1; i <= n; i++ {
			out = append(out, i)
		}
		return out, nil
	case SelectNone:
		return []int{}, nil
	}

	seen := make(map[int]bool, n)
	var out []int
	for _, item := range strings.Split(trimmed, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			return nil, fmt.Errorf("selection %q: empty item; write a list like 1,3-5", input)
		}
		if strings.EqualFold(item, SelectAll) || strings.EqualFold(item, SelectNone) {
			return nil, fmt.Errorf("selection %q: %s cannot be combined with other items", input, strings.ToLower(item))
		}

		lo, hi, err := parseItem(item, input)
		if err != nil {
			return nil, err
		}
		for i := lo; i <= hi; i++ {
			if i < 1 || i > n {
				return nil, fmt.Errorf("selection %q: %d is out of range 1-%d", input, i, n)
			}
			if seen[i] {
				return nil, fmt.Errorf("selection %q: %d is selected more than once", input, i)
			}
			seen[i] = true
			out = append(out, i)
		}
	}
	sort.Ints(out)
	return out, nil
}

// parseItem reads one item as an inclusive [lo, hi] range. A single index is
// the degenerate range [i, i].
func parseItem(item, input string) (int, int, error) {
	dash := strings.Index(item, "-")
	if dash < 0 {
		i, err := parseIndex(item, input)
		return i, i, err
	}
	loStr := strings.TrimSpace(item[:dash])
	hiStr := strings.TrimSpace(item[dash+1:])
	if loStr == "" || hiStr == "" {
		return 0, 0, fmt.Errorf("selection %q: %q is not a range; write it as low-high, e.g. 3-5", input, item)
	}
	lo, err := parseIndex(loStr, input)
	if err != nil {
		return 0, 0, err
	}
	hi, err := parseIndex(hiStr, input)
	if err != nil {
		return 0, 0, err
	}
	if lo > hi {
		return 0, 0, fmt.Errorf("selection %q: range %q counts backwards; write it as low-high", input, item)
	}
	return lo, hi, nil
}

// parseIndex reads a bare decimal index. Signs, decimals and any other
// decoration are rejected rather than being coerced by strconv.
func parseIndex(s, input string) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("selection %q: empty index", input)
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("selection %q: %q is not a number", input, s)
		}
	}
	i, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("selection %q: %q is not a usable number", input, s)
	}
	return i, nil
}

// Select returns the proposals named by a parsed selection. Indices are 1-based
// and are assumed to have come from ParseSelection with the same batch.
func Select(proposals []Proposal, indices []int) []Proposal {
	out := make([]Proposal, 0, len(indices))
	for _, i := range indices {
		if i >= 1 && i <= len(proposals) {
			out = append(out, proposals[i-1])
		}
	}
	return out
}
