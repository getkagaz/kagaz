package conventions

import (
	"reflect"
	"testing"

	"github.com/getkagaz/kagaz/internal/vaultkit/config"
)

// newConventions builds conventions from a vault.yaml body, so these tests
// exercise the same defaulting path a real vault goes through.
func newConventions(t *testing.T, yaml string) *Conventions {
	t.Helper()
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

const twoPeople = "people:\n  - name: Alex Rao\n    tag: alex-rao\n  - name: Sam Rao\n    tag: sam-rao\n"

// TestOwnerSeparatorDefaultIsUnambiguous pins the ruling that changed
// owner_groups.separator_filename's default from "-" to "+": with the default
// grammar, a filename says how many people own the document, and no lookup is
// needed to read it.
func TestOwnerSeparatorDefaultIsUnambiguous(t *testing.T) {
	cfg, err := config.Parse([]byte(twoPeople))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OwnerGroup.SeparatorFilename != "+" {
		t.Fatalf("default separator_filename = %q, want \"+\"", cfg.OwnerGroup.SeparatorFilename)
	}
	if cfg.OwnerGroup.SeparatorFilename == cfg.Filename.WordSeparator {
		t.Fatal("the owner separator must differ from the word separator, or filenames stop being invertible")
	}
}

func TestParseOwnersUnderTheDefaultGrammar(t *testing.T) {
	c := newConventions(t, twoPeople)
	cases := []struct {
		filename  string
		want      []string
		ambiguous bool
	}{
		{"Invoice_Alex-Rao_Acme-Corp_2026.pdf", []string{"Alex Rao"}, false},
		{"Invoice_Alex+Sam_Acme-Corp_2026.pdf", []string{"Alex", "Sam"}, false},
		{"Invoice_Alex-Rao+Sam-Rao_Acme-Corp_2026.pdf", []string{"Alex Rao", "Sam Rao"}, false},
		// Someone who is not in people: is taken exactly as written, never
		// invented into a person and never treated as ambiguous.
		{"Invoice_Jordan-Lee_Acme-Corp_2026.pdf", []string{"Jordan Lee"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.filename, func(t *testing.T) {
			doc, ok := c.Parse(tc.filename)
			if !ok {
				t.Fatal("filename did not parse")
			}
			if !reflect.DeepEqual(doc.Owners, tc.want) {
				t.Errorf("owners = %q, want %q", doc.Owners, tc.want)
			}
			if doc.OwnersAmbiguous != tc.ambiguous {
				t.Errorf("OwnersAmbiguous = %v, want %v", doc.OwnersAmbiguous, tc.ambiguous)
			}
		})
	}
}

// TestParseOwnersUnderAnAmbiguousGrammar covers the configuration a user may
// still choose: owner separator == word separator. There the people list is the
// only thing that can resolve a name, and what it cannot resolve is reported as
// ambiguous rather than guessed at.
func TestParseOwnersUnderAnAmbiguousGrammar(t *testing.T) {
	c := newConventions(t, twoPeople+"owner_groups:\n  separator_filename: \"-\"\n")
	cases := []struct {
		filename  string
		want      []string
		ambiguous bool
	}{
		{"Invoice_Alex-Rao_Acme-Corp_2026.pdf", []string{"Alex Rao"}, false},
		{"Invoice_Alex-Rao-Sam-Rao_Acme-Corp_2026.pdf", []string{"Alex Rao", "Sam Rao"}, false},
		{"Invoice_Jordan-Lee_Acme-Corp_2026.pdf", []string{"Jordan", "Lee"}, true},
		{"Invoice_Alex-Rao-Jordan_Acme-Corp_2026.pdf", []string{"Alex", "Rao", "Jordan"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.filename, func(t *testing.T) {
			doc, ok := c.Parse(tc.filename)
			if !ok {
				t.Fatal("filename did not parse")
			}
			if !reflect.DeepEqual(doc.Owners, tc.want) {
				t.Errorf("owners = %q, want %q", doc.Owners, tc.want)
			}
			if doc.OwnersAmbiguous != tc.ambiguous {
				t.Errorf("OwnersAmbiguous = %v, want %v", doc.OwnersAmbiguous, tc.ambiguous)
			}
		})
	}
}

func TestRenderParseRoundTrip(t *testing.T) {
	c := newConventions(t, twoPeople)
	for _, doc := range []Doc{
		{DocType: "invoice", Category: "financial", Owners: []string{"Alex Rao"}, Identifier: "Acme Corp", Year: 2026, Ext: ".pdf"},
		{DocType: "invoice", Category: "financial", Owners: []string{"Alex Rao", "Sam Rao"}, Identifier: "Acme Corp", Year: 2026, Ext: ".pdf"},
	} {
		name, err := c.Render(doc)
		if err != nil {
			t.Fatal(err)
		}
		back, ok := c.Parse(name)
		if !ok {
			t.Fatalf("%s did not parse back", name)
		}
		if !reflect.DeepEqual(back.Owners, doc.Owners) {
			t.Errorf("%s: owners round-tripped to %q, want %q", name, back.Owners, doc.Owners)
		}
		if back.OwnersAmbiguous {
			t.Errorf("%s: a name Kagaz rendered itself must not read back as ambiguous", name)
		}
	}
}

// TestResolveOwnersLongestFirst pins the deterministic tiebreak: the longest
// spelling that fits wins, so a person whose name is a prefix of another's does
// not capture the match.
func TestResolveOwnersLongestFirst(t *testing.T) {
	c := newConventions(t, "people:\n  - name: Alex\n  - name: Alex Rao\nowner_groups:\n  separator_filename: \"-\"\n")
	doc, ok := c.Parse("Invoice_Alex-Rao_Acme_2026.pdf")
	if !ok {
		t.Fatal("did not parse")
	}
	if !reflect.DeepEqual(doc.Owners, []string{"Alex Rao"}) {
		t.Errorf("owners = %q, want [\"Alex Rao\"]", doc.Owners)
	}
}
