package ingest

import (
	"strings"
	"testing"
	"time"

	"github.com/getkagaz/kagaz/internal/vaultkit/config"
)

func peopleConfig(t *testing.T, people ...config.Person) *config.Config {
	t.Helper()
	var b strings.Builder
	b.WriteString("people:\n")
	for _, p := range people {
		b.WriteString("  - name: " + p.Name + "\n")
		if p.Tag != "" {
			b.WriteString("    tag: " + p.Tag + "\n")
		}
	}
	cfg, err := config.Parse([]byte(b.String()))
	if err != nil {
		t.Fatalf("config.Parse: %v", err)
	}
	return cfg
}

func TestInferOwners(t *testing.T) {
	alex := config.Person{Name: "Alex Rao", Tag: "alex-rao"}
	sam := config.Person{Name: "Sam Rao", Tag: "sam-rao"}
	// A second Alex makes the given name ambiguous, which must stop the
	// given-name match entirely rather than pick one.
	alexKhan := config.Person{Name: "Alex Khan", Tag: "alex-khan"}

	tests := []struct {
		name       string
		people     []config.Person
		path       string
		text       string
		wantOwners []string
		wantSource string
	}{
		{
			name:       "full name in the document text",
			people:     []config.Person{alex, sam},
			path:       "/in/scan001.pdf",
			text:       "Bill To:\nAlex Rao\n44 Sample Street",
			wantOwners: []string{"Alex Rao"},
			wantSource: SourceText,
		},
		{
			name:       "full name in the file name wins over the text",
			people:     []config.Person{alex, sam},
			path:       "/in/Alex Rao insurance.pdf",
			text:       "Sam Rao is mentioned in the body",
			wantOwners: []string{"Alex Rao", "Sam Rao"},
			wantSource: SourceFilename,
		},
		{
			name:       "tag in the file name",
			people:     []config.Person{alex, sam},
			path:       "/in/alex-rao_payslip_2024.pdf",
			text:       "no names here",
			wantOwners: []string{"Alex Rao"},
			wantSource: SourceFilename,
		},
		{
			name:       "unique given name matches, and is marked as the weak match it is",
			people:     []config.Person{alex, sam},
			path:       "/in/alex passport.pdf",
			text:       "",
			wantOwners: []string{"Alex Rao"},
			wantSource: SourceGivenName,
		},
		{
			name:       "shared given name matches nobody",
			people:     []config.Person{alex, alexKhan},
			path:       "/in/alex passport.pdf",
			text:       "",
			wantOwners: nil,
			wantSource: SourceNone,
		},
		{
			name:       "a surname alone never matches",
			people:     []config.Person{alex, sam},
			path:       "/in/rao household bill.pdf",
			text:       "",
			wantOwners: nil,
			wantSource: SourceNone,
		},
		{
			name:       "a longer word containing the name does not match",
			people:     []config.Person{alex, sam},
			path:       "/in/alexandra.pdf",
			text:       "Alexandra Rao-Smithson attended",
			wantOwners: nil,
			wantSource: SourceNone,
		},
		{
			name:       "both people named means both own it",
			people:     []config.Person{alex, sam},
			path:       "/in/joint account.pdf",
			text:       "Account holders: Alex Rao and Sam Rao",
			wantOwners: []string{"Alex Rao", "Sam Rao"},
			wantSource: SourceText,
		},
		{
			name:       "punctuation between name parts still matches",
			people:     []config.Person{alex},
			path:       "/in/ALEX_RAO-2024.pdf",
			text:       "",
			wantOwners: []string{"Alex Rao"},
			wantSource: SourceFilename,
		},
		{
			name:       "nobody configured means nobody matched",
			people:     nil,
			path:       "/in/alex rao.pdf",
			text:       "Alex Rao",
			wantOwners: nil,
			wantSource: SourceNone,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := peopleConfig(t, tt.people...)
			owners, why := inferOwners(cfg, tt.path, tt.text)

			if strings.Join(owners, ",") != strings.Join(tt.wantOwners, ",") {
				t.Fatalf("owners = %v, want %v", owners, tt.wantOwners)
			}
			if len(why) == 0 {
				t.Fatal("no rationale recorded")
			}
			if why[0].Source != tt.wantSource {
				t.Errorf("first reason source = %q, want %q (%q)", why[0].Source, tt.wantSource, why[0].Detail)
			}
			for _, r := range why {
				if r.Detail == "" {
					t.Error("a reason with no explanation")
				}
			}
		})
	}
}

func TestInferYear(t *testing.T) {
	mod := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name       string
		fields     map[string]string
		wantYear   int
		wantSource string
	}{
		{
			name:       "date field",
			fields:     map[string]string{"date": "12/03/2024"},
			wantYear:   2024,
			wantSource: SourceField,
		},
		{
			name:       "date wins over a later-ranked field",
			fields:     map[string]string{"date": "12/03/2024", "tax_year": "2019-20"},
			wantYear:   2024,
			wantSource: SourceField,
		},
		{
			name:       "tax year when there is no date",
			fields:     map[string]string{"tax_year": "2019-20"},
			wantYear:   2019,
			wantSource: SourceField,
		},
		{
			name:       "written-out date",
			fields:     map[string]string{"date": "4 February 2019"},
			wantYear:   2019,
			wantSource: SourceField,
		},
		{
			name:       "an expiry date is never the document's year",
			fields:     map[string]string{"expiry": "03 Feb 2034"},
			wantYear:   2026,
			wantSource: SourceModTime,
		},
		{
			name:       "a due date is not used when it is the only field",
			fields:     map[string]string{"due_date": "26/03/2024"},
			wantYear:   2026,
			wantSource: SourceModTime,
		},
		{
			name:       "no fields at all",
			fields:     nil,
			wantYear:   2026,
			wantSource: SourceModTime,
		},
		{
			name:       "a field with no parsable year falls through",
			fields:     map[string]string{"date": "last Tuesday"},
			wantYear:   2026,
			wantSource: SourceModTime,
		},
		{
			name:       "an empty field value falls through",
			fields:     map[string]string{"date": "   "},
			wantYear:   2026,
			wantSource: SourceModTime,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			year, why := inferYear(tt.fields, mod)
			if year != tt.wantYear {
				t.Fatalf("year = %d, want %d (%s)", year, tt.wantYear, why.Detail)
			}
			if why.Source != tt.wantSource {
				t.Fatalf("source = %q, want %q", why.Source, tt.wantSource)
			}
			if why.Source == SourceModTime && !strings.Contains(why.Detail, "guess") {
				t.Errorf("an mtime year must be described as a guess: %q", why.Detail)
			}
			if why.Source == SourceField && !strings.Contains(why.Detail, "extracted") {
				t.Errorf("a field year must name its field: %q", why.Detail)
			}
		})
	}
}

func TestInferIdentifier(t *testing.T) {
	tests := []struct {
		name       string
		fields     map[string]string
		path       string
		docType    string
		owners     []string
		want       string
		wantSource string
	}{
		{
			name:       "a named issuer beats a reference number",
			fields:     map[string]string{"issuer": "Acme Corp", "invoice_number": "INV-1"},
			path:       "/in/scan.pdf",
			docType:    "invoice",
			want:       "Acme Corp",
			wantSource: SourceField,
		},
		{
			name:       "an account number beats an invoice number",
			fields:     map[string]string{"account_number": "AC-9911", "invoice_number": "INV-1"},
			path:       "/in/scan.pdf",
			docType:    "invoice",
			want:       "AC-9911",
			wantSource: SourceField,
		},
		{
			name:       "the file name is cleaned when nothing was extracted",
			path:       "/in/scan_2024-03-02_alex_acme corp invoice.pdf",
			docType:    "invoice",
			owners:     []string{"Alex Rao"},
			want:       "Acme Corp",
			wantSource: SourceFilename,
		},
		{
			name:       "scanner noise alone leaves nothing usable",
			path:       "/in/IMG_0042.pdf",
			docType:    "invoice",
			want:       UnknownIdentifier,
			wantSource: SourceNone,
		},
		{
			name:       "a date-only file name leaves nothing usable",
			path:       "/in/2024-03-02.pdf",
			docType:    "invoice",
			want:       UnknownIdentifier,
			wantSource: SourceNone,
		},
		{
			name:       "the doctype word is not repeated into the identifier",
			path:       "/in/passport.pdf",
			docType:    "passport",
			want:       UnknownIdentifier,
			wantSource: SourceNone,
		},
		{
			name:       "a multi-word doctype is stripped whole",
			path:       "/in/boarding pass ba.pdf",
			docType:    "boarding-pass",
			want:       "Ba",
			wantSource: SourceFilename,
		},
		{
			name:       "an empty field value is skipped, and the doctype words are stripped from the stem",
			fields:     map[string]string{"issuer": "  "},
			path:       "/in/globex policy.pdf",
			docType:    "insurance-policy",
			want:       "Globex",
			wantSource: SourceFilename,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, why := inferIdentifier(tt.fields, tt.path, tt.docType, tt.owners)
			if got != tt.want {
				t.Fatalf("identifier = %q, want %q (%s)", got, tt.want, why.Detail)
			}
			if why.Source != tt.wantSource {
				t.Fatalf("source = %q, want %q", why.Source, tt.wantSource)
			}
			if why.Detail == "" {
				t.Error("no explanation recorded")
			}
		})
	}
}

func TestNormalizeAndWordMatch(t *testing.T) {
	if got := normalizeForMatch("ALEX_Rao-2024!!"); got != "alex rao 2024" {
		t.Fatalf("normalizeForMatch = %q", got)
	}
	if !containsWord("alex rao 2024", "alex rao") {
		t.Error("containsWord missed an exact phrase")
	}
	if containsWord("alexandra rao", "alex") {
		t.Error("containsWord matched a prefix of a longer word")
	}
	if containsWord("", "alex") || containsWord("alex", "") {
		t.Error("containsWord matched an empty operand")
	}
}

func TestClipStaysOnRuneBoundaries(t *testing.T) {
	s := strings.Repeat("é", 10) // two bytes per rune
	for n := 0; n <= len(s); n++ {
		got := clip(s, n)
		if !utf8Valid(got) {
			t.Fatalf("clip(%d) produced invalid UTF-8: %q", n, got)
		}
	}
}

func utf8Valid(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}
