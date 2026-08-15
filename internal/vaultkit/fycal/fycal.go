// Package fycal implements configurable fiscal-year and quarter math. The
// fiscal year is defined by a start month (1 = calendar year, the global
// default) and a label template, so India (April), Australia (July) and the US
// federal year (October) are configuration rather than code.
package fycal

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Calendar computes fiscal periods for one vault.
type Calendar struct {
	StartMonth  int
	LabelFormat string
}

// New builds a Calendar, defaulting an unset start month to January.
func New(startMonth int, labelFormat string) Calendar {
	if startMonth < 1 || startMonth > 12 {
		startMonth = 1
	}
	if labelFormat == "" {
		if startMonth == 1 {
			labelFormat = "FY {yyyy1}"
		} else {
			labelFormat = "FY {yy1}-{yy2}"
		}
	}
	return Calendar{StartMonth: startMonth, LabelFormat: labelFormat}
}

// Year identifies one fiscal year by the calendar year it begins in.
type Year struct {
	Start int // calendar year the fiscal year begins in
	cal   Calendar
}

// Split reports whether the fiscal year straddles two calendar years.
func (y Year) Split() bool { return y.cal.StartMonth != 1 }

// End is the calendar year the fiscal year ends in.
func (y Year) End() int {
	if y.Split() {
		return y.Start + 1
	}
	return y.Start
}

// Label renders the fiscal year using the vault's label_format.
func (y Year) Label() string {
	r := strings.NewReplacer(
		"{yyyy1}", fmt.Sprintf("%04d", y.Start),
		"{yyyy2}", fmt.Sprintf("%04d", y.End()),
		"{yy1}", fmt.Sprintf("%02d", y.Start%100),
		"{yy2}", fmt.Sprintf("%02d", y.End()%100),
	)
	return r.Replace(y.cal.LabelFormat)
}

// Tag renders the fiscal year as a Finder-tag slug, e.g. "fy2026" or
// "fy2026-27". Tags are lowercase and separator-free by convention.
func (y Year) Tag() string {
	var b strings.Builder
	lastDash := true
	for _, r := range strings.ToLower(y.Label()) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// Range returns the half-open interval [from, to) covered by the fiscal year.
func (y Year) Range() (time.Time, time.Time) {
	from := time.Date(y.Start, time.Month(y.cal.StartMonth), 1, 0, 0, 0, 0, time.UTC)
	return from, from.AddDate(1, 0, 0)
}

// Contains reports whether t falls inside the fiscal year.
func (y Year) Contains(t time.Time) bool {
	from, to := y.Range()
	u := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	return !u.Before(from) && u.Before(to)
}

// String is the human label.
func (y Year) String() string { return y.Label() }

// YearOf returns the fiscal year containing t.
func (c Calendar) YearOf(t time.Time) Year {
	start := t.Year()
	if int(t.Month()) < c.StartMonth {
		start--
	}
	return Year{Start: start, cal: c}
}

// YearStarting returns the fiscal year beginning in calendar year y.
func (c Calendar) YearStarting(y int) Year { return Year{Start: y, cal: c} }

// Quarter is a fiscal quarter, 1-4, counted from the fiscal start month.
type Quarter struct {
	Year   Year
	Number int
}

// Label renders the quarter, e.g. "FY 2026 Q3".
func (q Quarter) Label() string { return fmt.Sprintf("%s Q%d", q.Year.Label(), q.Number) }

// Range returns the half-open interval [from, to) covered by the quarter.
func (q Quarter) Range() (time.Time, time.Time) {
	from, _ := q.Year.Range()
	from = from.AddDate(0, 3*(q.Number-1), 0)
	return from, from.AddDate(0, 3, 0)
}

// QuarterOf returns the fiscal quarter containing t.
func (c Calendar) QuarterOf(t time.Time) Quarter {
	y := c.YearOf(t)
	// Months elapsed since the fiscal year began, 0-11.
	elapsed := (int(t.Month()) - c.StartMonth + 12) % 12
	return Quarter{Year: y, Number: elapsed/3 + 1}
}

// Period is a resolved time window from a user-supplied period expression.
type Period struct {
	Label string
	From  time.Time // inclusive
	To    time.Time // exclusive
}

// Contains reports whether t falls inside the period.
func (p Period) Contains(t time.Time) bool {
	u := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	return !u.Before(p.From) && u.Before(p.To)
}

// ParsePeriod resolves a period expression against the vault calendar.
//
// Accepted forms:
//
//	2026            calendar year
//	2026-05         calendar month
//	2026-05-11      single day
//	FY2026, fy2026  fiscal year starting in 2026
//	FY2026-27       fiscal year starting in 2026 (end year is checked)
//	FY2026Q3        fiscal quarter
//	2026Q3, 2026-Q3 fiscal quarter
func (c Calendar) ParsePeriod(expr string) (Period, error) {
	s := strings.TrimSpace(expr)
	if s == "" {
		return Period{}, fmt.Errorf("empty period")
	}
	upper := strings.ToUpper(s)

	if strings.HasPrefix(upper, "FY") {
		rest := strings.TrimSpace(strings.TrimPrefix(upper, "FY"))
		return c.parseFiscal(rest, s)
	}
	// A bare year with a quarter suffix is fiscal too: quarters only make sense
	// relative to the fiscal calendar.
	if i := strings.Index(upper, "Q"); i > 0 {
		return c.parseFiscal(upper, s)
	}

	switch parts := strings.Split(s, "-"); len(parts) {
	case 1:
		y, err := strconv.Atoi(parts[0])
		if err != nil {
			return Period{}, fmt.Errorf("unrecognised period %q", expr)
		}
		from := time.Date(y, time.January, 1, 0, 0, 0, 0, time.UTC)
		return Period{Label: parts[0], From: from, To: from.AddDate(1, 0, 0)}, nil
	case 2:
		y, err1 := strconv.Atoi(parts[0])
		m, err2 := strconv.Atoi(parts[1])
		if err1 != nil || err2 != nil || m < 1 || m > 12 {
			return Period{}, fmt.Errorf("unrecognised period %q", expr)
		}
		from := time.Date(y, time.Month(m), 1, 0, 0, 0, 0, time.UTC)
		return Period{Label: s, From: from, To: from.AddDate(0, 1, 0)}, nil
	case 3:
		t, err := time.Parse("2006-01-02", s)
		if err != nil {
			return Period{}, fmt.Errorf("unrecognised period %q", expr)
		}
		return Period{Label: s, From: t, To: t.AddDate(0, 0, 1)}, nil
	default:
		return Period{}, fmt.Errorf("unrecognised period %q", expr)
	}
}

// parseFiscal handles the post-"FY" remainder plus bare "2026Q3" forms.
func (c Calendar) parseFiscal(rest, original string) (Period, error) {
	quarter := 0
	if i := strings.Index(rest, "Q"); i >= 0 {
		q, err := strconv.Atoi(strings.TrimSpace(rest[i+1:]))
		if err != nil || q < 1 || q > 4 {
			return Period{}, fmt.Errorf("unrecognised quarter in %q", original)
		}
		quarter = q
		rest = strings.TrimRight(strings.TrimSpace(rest[:i]), "-")
	}

	yearPart := rest
	endPart := ""
	if i := strings.Index(rest, "-"); i >= 0 {
		yearPart, endPart = rest[:i], rest[i+1:]
	}
	start, err := strconv.Atoi(strings.TrimSpace(yearPart))
	if err != nil {
		return Period{}, fmt.Errorf("unrecognised fiscal year in %q", original)
	}
	if start < 100 {
		start += 2000
	}
	y := c.YearStarting(start)
	if endPart != "" {
		end, err := strconv.Atoi(strings.TrimSpace(endPart))
		if err != nil {
			return Period{}, fmt.Errorf("unrecognised fiscal year in %q", original)
		}
		if end < 100 {
			end += (start / 100) * 100
		}
		if end != y.End() {
			return Period{}, fmt.Errorf("%q ends in %d but the vault fiscal year starting %d ends in %d", original, end, start, y.End())
		}
	}

	if quarter > 0 {
		q := Quarter{Year: y, Number: quarter}
		from, to := q.Range()
		return Period{Label: q.Label(), From: from, To: to}, nil
	}
	from, to := y.Range()
	return Period{Label: y.Label(), From: from, To: to}, nil
}
