package ingest

import (
	"reflect"
	"strings"
	"testing"
)

// TestParseSelection is deliberately exhaustive. This function stands between
// somebody typing at a prompt and a bulk file move: a wrong parse moves the
// wrong documents, and every malformed form below is one somebody will type.
func TestParseSelection(t *testing.T) {
	tests := []struct {
		name  string
		input string
		n     int
		want  []int
		err   string // substring the error must contain; "" means success
	}{
		// -- whole-batch answers --------------------------------------------
		{name: "all", input: "all", n: 3, want: []int{1, 2, 3}},
		{name: "all uppercase", input: "ALL", n: 2, want: []int{1, 2}},
		{name: "all padded", input: "  all  ", n: 2, want: []int{1, 2}},
		{name: "none", input: "none", n: 3, want: []int{}},
		{name: "none mixed case", input: "None", n: 3, want: []int{}},
		{name: "all over an empty batch", input: "all", n: 0, want: []int{}},
		{name: "none over an empty batch", input: "none", n: 0, want: []int{}},

		// -- single indices --------------------------------------------------
		{name: "single", input: "2", n: 3, want: []int{2}},
		{name: "single padded", input: " 2 ", n: 3, want: []int{2}},
		{name: "first", input: "1", n: 1, want: []int{1}},
		{name: "leading zeros are accepted", input: "01", n: 3, want: []int{1}},

		// -- lists and ranges -------------------------------------------------
		{name: "list", input: "1,3", n: 5, want: []int{1, 3}},
		{name: "range", input: "3-5", n: 5, want: []int{3, 4, 5}},
		{name: "list and range", input: "1,3-5", n: 5, want: []int{1, 3, 4, 5}},
		{name: "unsorted input is sorted", input: "5,1,3", n: 5, want: []int{1, 3, 5}},
		{name: "whitespace everywhere", input: " 1 , 3 - 5 ", n: 5, want: []int{1, 3, 4, 5}},
		{name: "single-element range", input: "2-2", n: 3, want: []int{2}},
		{name: "whole batch as a range", input: "1-4", n: 4, want: []int{1, 2, 3, 4}},

		// -- refusals ---------------------------------------------------------
		{name: "empty", input: "", n: 3, err: "empty"},
		{name: "whitespace only", input: "   ", n: 3, err: "empty"},
		{name: "zero", input: "0", n: 3, err: "out of range"},
		{name: "above the batch", input: "4", n: 3, err: "out of range"},
		{name: "range past the end", input: "2-9", n: 3, err: "out of range"},
		{name: "range starting at zero", input: "0-2", n: 3, err: "out of range"},
		{name: "any index over an empty batch", input: "1", n: 0, err: "out of range"},
		{name: "reversed range", input: "5-3", n: 6, err: "backwards"},
		{name: "duplicate index", input: "1,1", n: 3, err: "more than once"},
		{name: "overlapping ranges", input: "1-3,2-4", n: 5, err: "more than once"},
		{name: "index inside an earlier range", input: "1-3,2", n: 5, err: "more than once"},
		{name: "negative", input: "-1", n: 3, err: "not a range"},
		{name: "open-ended range", input: "1-", n: 3, err: "not a range"},
		{name: "double dash", input: "1--3", n: 3, err: "not a number"},
		{name: "trailing comma", input: "1,", n: 3, err: "empty item"},
		{name: "leading comma", input: ",1", n: 3, err: "empty item"},
		{name: "double comma", input: "1,,2", n: 3, err: "empty item"},
		{name: "not a number", input: "one", n: 3, err: "not a number"},
		{name: "decimal", input: "1.5", n: 3, err: "not a number"},
		{name: "signed", input: "+1", n: 3, err: "not a number"},
		{name: "hex", input: "0x2", n: 3, err: "not a number"},
		{name: "spaced digits", input: "1 2", n: 3, err: "not a number"},
		{name: "all combined with an index", input: "all,2", n: 3, err: "cannot be combined"},
		{name: "none combined with an index", input: "none,2", n: 3, err: "cannot be combined"},
		{name: "index combined with all", input: "2,all", n: 3, err: "cannot be combined"},
		{name: "semicolons are not separators", input: "1;2", n: 3, err: "not a number"},
		{name: "range with spaces inside a number", input: "1 - 2 3", n: 5, err: "not a number"},
		{name: "absurdly long number", input: strings.Repeat("9", 40), n: 3, err: "not a usable number"},
		{name: "negative batch size", input: "all", n: -1, err: "negative"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseSelection(tt.input, tt.n)
			if tt.err != "" {
				if err == nil {
					t.Fatalf("ParseSelection(%q, %d) = %v, want an error containing %q", tt.input, tt.n, got, tt.err)
				}
				if !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("error = %q, want it to contain %q", err, tt.err)
				}
				if got != nil {
					t.Errorf("a rejected selection still returned %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseSelection(%q, %d): %v", tt.input, tt.n, err)
			}
			if len(got) == 0 && len(tt.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("ParseSelection(%q, %d) = %v, want %v", tt.input, tt.n, got, tt.want)
			}
		})
	}
}

// TestSelectPicksTheNamedProposals ties the parsed indices back to the batch,
// which is the step where an off-by-one would move the wrong document.
func TestSelectPicksTheNamedProposals(t *testing.T) {
	batch := []Proposal{
		{Index: 1, Source: "a"},
		{Index: 2, Source: "b"},
		{Index: 3, Source: "c"},
		{Index: 4, Source: "d"},
	}
	idx, err := ParseSelection("1,3-4", len(batch))
	if err != nil {
		t.Fatal(err)
	}
	got := Select(batch, idx)
	if len(got) != 3 || got[0].Source != "a" || got[1].Source != "c" || got[2].Source != "d" {
		t.Fatalf("Select = %+v, want a, c, d", got)
	}

	idx, err = ParseSelection("none", len(batch))
	if err != nil {
		t.Fatal(err)
	}
	if got := Select(batch, idx); len(got) != 0 {
		t.Fatalf("Select(none) = %+v, want nothing", got)
	}
}
