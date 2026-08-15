package fycal

import (
	"testing"
	"time"
)

func day(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func TestNewDefaults(t *testing.T) {
	tests := []struct {
		name       string
		start      int
		format     string
		wantStart  int
		wantFormat string
	}{
		{"zero start month becomes January", 0, "", 1, "FY {yyyy1}"},
		{"out-of-range start month becomes January", 13, "", 1, "FY {yyyy1}"},
		{"negative start month becomes January", -3, "", 1, "FY {yyyy1}"},
		{"split year gets the split default label", 4, "", 4, "FY {yy1}-{yy2}"},
		{"an explicit format is kept", 7, "{yyyy1}/{yyyy2}", 7, "{yyyy1}/{yyyy2}"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := New(tt.start, tt.format)
			if c.StartMonth != tt.wantStart || c.LabelFormat != tt.wantFormat {
				t.Fatalf("New(%d, %q) = %+v, want {%d %q}", tt.start, tt.format, c, tt.wantStart, tt.wantFormat)
			}
		})
	}
}

func TestYearLabelAndEnd(t *testing.T) {
	tests := []struct {
		name      string
		start     int
		format    string
		year      int
		wantLabel string
		wantEnd   int
		wantSplit bool
	}{
		{"calendar year", 1, "", 2026, "FY 2026", 2026, false},
		{"april start, two-digit label", 4, "", 2026, "FY 26-27", 2027, true},
		{"july start, four-digit label", 7, "FY {yyyy1}-{yyyy2}", 2026, "FY 2026-2027", 2027, true},
		{"october start", 10, "", 1999, "FY 99-00", 2000, true},
		{"century rollover keeps four digits", 1, "FY {yyyy1}", 2000, "FY 2000", 2000, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			y := New(tt.start, tt.format).YearStarting(tt.year)
			if got := y.Label(); got != tt.wantLabel {
				t.Errorf("Label() = %q, want %q", got, tt.wantLabel)
			}
			if got := y.End(); got != tt.wantEnd {
				t.Errorf("End() = %d, want %d", got, tt.wantEnd)
			}
			if got := y.Split(); got != tt.wantSplit {
				t.Errorf("Split() = %v, want %v", got, tt.wantSplit)
			}
			if got := y.String(); got != tt.wantLabel {
				t.Errorf("String() = %q, want the label %q", got, tt.wantLabel)
			}
		})
	}
}

// TestYearTag pins the tag spelling. This is the function ingest uses to
// propose a fiscal-year Finder tag, and the tag it produces has to be one a
// vault's `tags.fiscal_years` list can plausibly contain: whitespace in the
// label is typographic and disappears, while a dash the user wrote in their
// label_format is a real separator and survives.
func TestYearTag(t *testing.T) {
	tests := []struct {
		name   string
		start  int
		format string
		year   int
		want   string
	}{
		{"calendar default", 1, "", 2026, "fy2026"},
		{"calendar default, earlier year", 1, "", 2024, "fy2024"},
		{"split default keeps the literal dash", 4, "", 2025, "fy25-26"},
		{"four-digit start, two-digit end", 4, "FY {yyyy1}-{yy2}", 2026, "fy2026-27"},
		{"four-digit both ends", 4, "FY {yyyy1}-{yyyy2}", 2026, "fy2026-2027"},
		{"no space in the format", 1, "FY{yyyy1}", 2026, "fy2026"},
		{"multiple spaces collapse to nothing", 1, "FY   {yyyy1}", 2026, "fy2026"},
		{"slash separator becomes a dash", 1, "{yyyy1}/Q", 2026, "2026-q"},
		{"leading and trailing punctuation is trimmed", 1, "[{yyyy1}]", 2026, "2026"},
		{"lowercase already", 1, "fy {yyyy1}", 2026, "fy2026"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := New(tt.start, tt.format).YearStarting(tt.year).Tag()
			if got != tt.want {
				t.Fatalf("Tag() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestYearTagMatchesShippedVocabularies is the regression the old behaviour
// could not survive: both examples/vault.yaml and the fixture vault list
// fiscal-year tags in the "fy2026" spelling, and a tag ingest proposes has to
// be in the vault's controlled vocabulary or it is silently dropped.
func TestYearTagMatchesShippedVocabularies(t *testing.T) {
	cal := New(1, "FY {yyyy1}")
	for year, want := range map[int]string{2024: "fy2024", 2025: "fy2025", 2026: "fy2026"} {
		if got := cal.YearStarting(year).Tag(); got != want {
			t.Errorf("Tag() for %d = %q, want %q", year, got, want)
		}
	}
}

func TestYearRangeAndContains(t *testing.T) {
	tests := []struct {
		name     string
		start    int
		year     int
		wantFrom time.Time
		wantTo   time.Time
		in       []time.Time
		out      []time.Time
	}{
		{
			name:     "calendar year",
			start:    1,
			year:     2026,
			wantFrom: day(2026, time.January, 1),
			wantTo:   day(2027, time.January, 1),
			in:       []time.Time{day(2026, time.January, 1), day(2026, time.December, 31)},
			out:      []time.Time{day(2025, time.December, 31), day(2027, time.January, 1)},
		},
		{
			name:     "april year straddles the calendar boundary",
			start:    4,
			year:     2026,
			wantFrom: day(2026, time.April, 1),
			wantTo:   day(2027, time.April, 1),
			in:       []time.Time{day(2026, time.April, 1), day(2026, time.December, 31), day(2027, time.March, 31)},
			out:      []time.Time{day(2026, time.March, 31), day(2027, time.April, 1)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			y := New(tt.start, "").YearStarting(tt.year)
			from, to := y.Range()
			if !from.Equal(tt.wantFrom) || !to.Equal(tt.wantTo) {
				t.Fatalf("Range() = [%s, %s), want [%s, %s)", from, to, tt.wantFrom, tt.wantTo)
			}
			for _, at := range tt.in {
				if !y.Contains(at) {
					t.Errorf("Contains(%s) = false, want true", at.Format("2006-01-02"))
				}
			}
			for _, at := range tt.out {
				if y.Contains(at) {
					t.Errorf("Contains(%s) = true, want false", at.Format("2006-01-02"))
				}
			}
		})
	}
}

// TestYearOfBoundaries is the off-by-one that a split fiscal year invites:
// the day before the fiscal year starts belongs to the previous one.
func TestYearOfBoundaries(t *testing.T) {
	tests := []struct {
		name  string
		start int
		at    time.Time
		want  int
	}{
		{"calendar: first day", 1, day(2026, time.January, 1), 2026},
		{"calendar: last day", 1, day(2026, time.December, 31), 2026},
		{"calendar: day before", 1, day(2025, time.December, 31), 2025},
		{"april: the day before the start", 4, day(2026, time.March, 31), 2025},
		{"april: the first day", 4, day(2026, time.April, 1), 2026},
		{"april: december falls in the year that began in april", 4, day(2026, time.December, 1), 2026},
		{"april: the last day", 4, day(2027, time.March, 31), 2026},
		{"october: september is the previous year", 10, day(2026, time.September, 30), 2025},
		{"october: october starts the new year", 10, day(2026, time.October, 1), 2026},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := New(tt.start, "").YearOf(tt.at).Start; got != tt.want {
				t.Fatalf("YearOf(%s).Start = %d, want %d", tt.at.Format("2006-01-02"), got, tt.want)
			}
		})
	}
}

func TestQuarterOfBoundaries(t *testing.T) {
	tests := []struct {
		name      string
		start     int
		at        time.Time
		wantYear  int
		wantQ     int
		wantLabel string
	}{
		{"calendar Q1 first day", 1, day(2026, time.January, 1), 2026, 1, "FY 2026 Q1"},
		{"calendar Q1 last day", 1, day(2026, time.March, 31), 2026, 1, "FY 2026 Q1"},
		{"calendar Q2 first day", 1, day(2026, time.April, 1), 2026, 2, "FY 2026 Q2"},
		{"calendar Q4 last day", 1, day(2026, time.December, 31), 2026, 4, "FY 2026 Q4"},
		{"april Q1 starts in april", 4, day(2026, time.April, 1), 2026, 1, "FY 26-27 Q1"},
		{"april Q3 covers october", 4, day(2026, time.October, 1), 2026, 3, "FY 26-27 Q3"},
		{"april Q4 covers january of the next calendar year", 4, day(2027, time.January, 15), 2026, 4, "FY 26-27 Q4"},
		{"april Q4 last day", 4, day(2027, time.March, 31), 2026, 4, "FY 26-27 Q4"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := New(tt.start, "").QuarterOf(tt.at)
			if q.Year.Start != tt.wantYear || q.Number != tt.wantQ {
				t.Fatalf("QuarterOf(%s) = FY%d Q%d, want FY%d Q%d",
					tt.at.Format("2006-01-02"), q.Year.Start, q.Number, tt.wantYear, tt.wantQ)
			}
			if got := q.Label(); got != tt.wantLabel {
				t.Errorf("Label() = %q, want %q", got, tt.wantLabel)
			}
			from, to := q.Range()
			if !q.Year.Contains(from) || !to.After(from) {
				t.Errorf("Range() = [%s, %s), which is not inside the fiscal year", from, to)
			}
			if !(!tt.at.Before(from) && tt.at.Before(to)) {
				t.Errorf("%s is not inside its own quarter's range [%s, %s)", tt.at, from, to)
			}
		})
	}
}

func TestParsePeriod(t *testing.T) {
	tests := []struct {
		name     string
		start    int
		expr     string
		wantFrom time.Time
		wantTo   time.Time
		wantErr  bool
	}{
		{name: "calendar year", start: 1, expr: "2026", wantFrom: day(2026, time.January, 1), wantTo: day(2027, time.January, 1)},
		{name: "calendar month", start: 1, expr: "2026-05", wantFrom: day(2026, time.May, 1), wantTo: day(2026, time.June, 1)},
		{name: "single day", start: 1, expr: "2026-05-11", wantFrom: day(2026, time.May, 11), wantTo: day(2026, time.May, 12)},
		{name: "december month rolls the year", start: 1, expr: "2026-12", wantFrom: day(2026, time.December, 1), wantTo: day(2027, time.January, 1)},

		// Both tag spellings must resolve, which is what lets the tag change
		// above be safe for anybody who already typed the old form.
		{name: "fiscal year, no separator", start: 4, expr: "FY2026", wantFrom: day(2026, time.April, 1), wantTo: day(2027, time.April, 1)},
		{name: "fiscal year, lowercase", start: 4, expr: "fy2026", wantFrom: day(2026, time.April, 1), wantTo: day(2027, time.April, 1)},
		{name: "fiscal year with end year", start: 4, expr: "FY2026-27", wantFrom: day(2026, time.April, 1), wantTo: day(2027, time.April, 1)},
		{name: "two-digit fiscal year", start: 4, expr: "FY26", wantFrom: day(2026, time.April, 1), wantTo: day(2027, time.April, 1)},
		{name: "calendar fiscal year", start: 1, expr: "FY2026", wantFrom: day(2026, time.January, 1), wantTo: day(2027, time.January, 1)},

		{name: "fiscal quarter", start: 4, expr: "FY2026Q3", wantFrom: day(2026, time.October, 1), wantTo: day(2027, time.January, 1)},
		{name: "bare year with quarter is fiscal", start: 4, expr: "2026Q1", wantFrom: day(2026, time.April, 1), wantTo: day(2026, time.July, 1)},
		{name: "dashed quarter", start: 1, expr: "2026-Q2", wantFrom: day(2026, time.April, 1), wantTo: day(2026, time.July, 1)},

		{name: "empty", start: 1, expr: "", wantErr: true},
		{name: "whitespace only", start: 1, expr: "   ", wantErr: true},
		{name: "not a period", start: 1, expr: "last tuesday", wantErr: true},
		{name: "month 13", start: 1, expr: "2026-13", wantErr: true},
		{name: "month 0", start: 1, expr: "2026-00", wantErr: true},
		{name: "impossible day", start: 1, expr: "2026-02-31", wantErr: true},
		{name: "too many parts", start: 1, expr: "2026-01-02-03", wantErr: true},
		{name: "quarter 0", start: 1, expr: "FY2026Q0", wantErr: true},
		{name: "quarter 5", start: 1, expr: "FY2026Q5", wantErr: true},
		{name: "quarter not a number", start: 1, expr: "FY2026Qx", wantErr: true},
		{name: "fiscal year with the wrong end year", start: 4, expr: "FY2026-28", wantErr: true},
		{name: "calendar vault rejects a split spelling", start: 1, expr: "FY2026-27", wantErr: true},
		{name: "fiscal year that is not a number", start: 1, expr: "FYnext", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := New(tt.start, "").ParsePeriod(tt.expr)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParsePeriod(%q) = %+v, want an error", tt.expr, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParsePeriod(%q): %v", tt.expr, err)
			}
			if !got.From.Equal(tt.wantFrom) || !got.To.Equal(tt.wantTo) {
				t.Fatalf("ParsePeriod(%q) = [%s, %s), want [%s, %s)",
					tt.expr, got.From.Format("2006-01-02"), got.To.Format("2006-01-02"),
					tt.wantFrom.Format("2006-01-02"), tt.wantTo.Format("2006-01-02"))
			}
			if got.Label == "" {
				t.Error("period has no label")
			}
			if !got.Contains(tt.wantFrom) {
				t.Error("period does not contain its own first day")
			}
			if got.Contains(tt.wantTo) {
				t.Error("period contains its exclusive end")
			}
		})
	}
}

// TestParsePeriodAcceptsWhatTagProduces closes the loop between the two halves
// of this package: every tag Tag() emits must be a period ParsePeriod accepts,
// or `kagaz find --period fy2026` would not find what ingest tagged.
func TestParsePeriodAcceptsWhatTagProduces(t *testing.T) {
	for _, start := range []int{1, 4, 7, 10} {
		cal := New(start, "")
		// Two-digit labels before 2000 are excluded: see
		// TestParsePeriodTwoDigitYearsAssumeThe2000s.
		for _, year := range []int{2024, 2026, 2030} {
			tag := cal.YearStarting(year).Tag()
			got, err := cal.ParsePeriod(tag)
			if err != nil {
				t.Errorf("start month %d, year %d: ParsePeriod(%q): %v", start, year, tag, err)
				continue
			}
			wantFrom, wantTo := cal.YearStarting(year).Range()
			if !got.From.Equal(wantFrom) || !got.To.Equal(wantTo) {
				t.Errorf("start month %d: ParsePeriod(%q) = [%s, %s), want [%s, %s)",
					start, tag, got.From, got.To, wantFrom, wantTo)
			}
		}
	}
}

// TestParsePeriodTwoDigitYearsAssumeThe2000s documents a pre-existing
// limitation this task did not introduce and did not fix: parseFiscal maps a
// bare two-digit year into the 2000s, so a split vault's 1999 tag ("fy99-00",
// which Tag has always produced) does not round-trip. It is recorded as a test
// rather than left as folklore.
func TestParsePeriodTwoDigitYearsAssumeThe2000s(t *testing.T) {
	cal := New(4, "")
	if got := cal.YearStarting(1999).Tag(); got != "fy99-00" {
		t.Fatalf("Tag() = %q, want fy99-00", got)
	}
	if _, err := cal.ParsePeriod("fy99-00"); err == nil {
		t.Fatal("ParsePeriod(\"fy99-00\") now succeeds; if that was fixed deliberately, delete this test")
	}
	// The 2000s spelling of the same shape does round-trip.
	if _, err := cal.ParsePeriod("fy26-27"); err != nil {
		t.Fatalf("ParsePeriod(\"fy26-27\"): %v", err)
	}
}
