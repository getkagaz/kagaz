package doctypes

import (
	"sort"
	"strings"
)

// Match is the result of rules-based classification.
type Match struct {
	DocType    string
	Category   string
	Confidence float64
	Signals    []string // the keywords/patterns that fired, for explainability
}

// Score is one doctype's rules score against a document.
type Score struct {
	DocType  *DocType
	Hits     int
	Patterns int
	Signals  []string
}

// weight of a structural pattern hit relative to a keyword hit. A machine
// readable zone or a formatted identifier is much stronger evidence than a word.
const patternWeight = 3

// Classify scores text against the catalog and returns the best match. It is
// deliberately conservative: when no doctype clears the evidence bar it returns
// Unclassified with zero confidence rather than guessing, because a wrong
// doctype silently misfiles a document.
func (c *Catalog) Classify(text string) Match {
	scores := c.Score(text)
	if len(scores) == 0 {
		return Match{DocType: Unclassified, Confidence: 0}
	}

	best := scores[0]
	bestWeight := best.Hits + best.Patterns*patternWeight

	// Evidence bar: two distinct keywords, or one structural pattern.
	if best.Patterns == 0 && best.Hits < 2 {
		return Match{DocType: Unclassified, Confidence: 0}
	}

	runnerUp := 0
	if len(scores) > 1 {
		runnerUp = scores[1].Hits + scores[1].Patterns*patternWeight
	}

	// Confidence combines absolute evidence with the margin over the runner-up.
	// A document that matches two doctypes equally well is not a confident call
	// even if both matched strongly.
	evidence := float64(bestWeight) / float64(bestWeight+3)
	margin := 1.0
	if bestWeight > 0 {
		margin = float64(bestWeight-runnerUp) / float64(bestWeight)
	}
	confidence := evidence * (0.5 + 0.5*margin)
	if confidence > 0.95 {
		confidence = 0.95 // rules never claim near-certainty; that is the model's job
	}

	return Match{
		DocType:    best.DocType.Name,
		Category:   best.DocType.Category,
		Confidence: confidence,
		Signals:    best.Signals,
	}
}

// Score returns every doctype that matched, strongest first.
func (c *Catalog) Score(text string) []Score {
	lower := strings.ToLower(text)
	var out []Score
	for _, dt := range c.types {
		s := Score{DocType: dt}
		for i, re := range dt.keywordRes {
			if re.MatchString(lower) {
				s.Hits++
				s.Signals = append(s.Signals, dt.Keywords[i])
			}
		}
		for _, re := range dt.Patterns {
			if re.MatchString(text) {
				s.Patterns++
				s.Signals = append(s.Signals, "pattern:"+re.String())
			}
		}
		if s.Hits > 0 || s.Patterns > 0 {
			out = append(out, s)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		wi := out[i].Hits + out[i].Patterns*patternWeight
		wj := out[j].Hits + out[j].Patterns*patternWeight
		if wi != wj {
			return wi > wj
		}
		// Tie-break on name so results are deterministic across runs.
		return out[i].DocType.Name < out[j].DocType.Name
	})
	return out
}

// ExtractFields runs the doctype's extraction templates over text. Missing
// fields are simply absent; extraction never fails the pipeline.
func (c *Catalog) ExtractFields(doctype, text string) map[string]string {
	dt, ok := c.Get(doctype)
	if !ok {
		return nil
	}
	return dt.ExtractFields(text)
}

// ExtractFields runs this doctype's extraction templates over text.
func (dt *DocType) ExtractFields(text string) map[string]string {
	if len(dt.Extract) == 0 {
		return nil
	}
	out := map[string]string{}
	for field, re := range dt.Extract {
		if m := re.FindStringSubmatch(text); len(m) > 1 {
			v := strings.TrimSpace(m[1])
			if v != "" {
				out[field] = v
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
