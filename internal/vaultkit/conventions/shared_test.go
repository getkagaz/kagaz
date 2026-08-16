package conventions

import (
	"reflect"
	"testing"
)

// TestParseSharedLabelIsNotAPerson pins the ruling that a {Names} field holding
// a category's shared label means "nobody owns this", not "a person called
// Shared". Without it every unowned document in a vault that configures
// `shared:` reads back with a phantom owner.
func TestParseSharedLabelIsNotAPerson(t *testing.T) {
	cases := []struct {
		name     string
		label    string
		filename string
	}{
		{"leading underscore", "_Shared", "Passport_Shared_Home-Office_2026.pdf"},
		{"bare word", "Shared", "Passport_Shared_Home-Office_2026.pdf"},
		{"punctuation on both sides", "-shared-", "Passport_shared_Home-Office_2026.pdf"},
		{"multi word label", "_Whole House", "Passport_Whole-House_Home-Office_2026.pdf"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newConventions(t, "structure:\n  identity:\n    path: Identity\n    shared: \""+tc.label+"\"\n")
			doc, ok := c.Parse(tc.filename)
			if !ok {
				t.Fatalf("%s did not parse", tc.filename)
			}
			if len(doc.Owners) != 0 {
				t.Errorf("owners = %q, want none: %q is the shared label, not a person", doc.Owners, tc.label)
			}
			if doc.OwnersAmbiguous {
				t.Error("a shared label is definitively unowned, not ambiguous")
			}
		})
	}
}

// TestParseSharedLabelDoesNotSwallowARealPerson pins the precedence: the check
// is against the whole {Names} value, so a configured person whose name merely
// contains the label still parses as that person.
func TestParseSharedLabelDoesNotSwallowARealPerson(t *testing.T) {
	c := newConventions(t, "people:\n  - name: Shared Services Ltd\n    tag: shared-services\n"+
		"structure:\n  identity:\n    path: Identity\n    shared: _Shared\n")

	doc, ok := c.Parse("Passport_Shared-Services-Ltd_Home-Office_2026.pdf")
	if !ok {
		t.Fatal("did not parse")
	}
	if !reflect.DeepEqual(doc.Owners, []string{"Shared Services Ltd"}) {
		t.Errorf("owners = %q, want [\"Shared Services Ltd\"]", doc.Owners)
	}

	// And the label itself is still unowned in the same vault.
	doc, ok = c.Parse("Passport_Shared_Home-Office_2026.pdf")
	if !ok {
		t.Fatal("did not parse")
	}
	if len(doc.Owners) != 0 {
		t.Errorf("owners = %q, want none", doc.Owners)
	}
}

// TestParseWithoutAnySharedLabelIsUnchanged: a vault where no category defines
// `shared:` has no labels to match, so "Shared" is just a name.
func TestParseWithoutAnySharedLabelIsUnchanged(t *testing.T) {
	c := newConventions(t, "structure:\n  identity:\n    path: Identity\n")
	doc, ok := c.Parse("Passport_Shared_Home-Office_2026.pdf")
	if !ok {
		t.Fatal("did not parse")
	}
	if !reflect.DeepEqual(doc.Owners, []string{"Shared"}) {
		t.Errorf("owners = %q, want [\"Shared\"]: this vault configures no shared label", doc.Owners)
	}
}

// TestParseMatchesEverySharedLabel: categories may disagree about the label,
// and Parse has only the filename, so all configured labels count.
func TestParseMatchesEverySharedLabel(t *testing.T) {
	c := newConventions(t, "structure:\n"+
		"  identity:\n    path: Identity\n    shared: _Shared\n"+
		"  property:\n    path: Property\n    shared: _Household\n"+
		"  vehicles:\n    path: Vehicles\n")
	for _, name := range []string{
		"Passport_Shared_Home-Office_2026.pdf",
		"Deed_Household_Land-Registry_2026.pdf",
	} {
		doc, ok := c.Parse(name)
		if !ok {
			t.Fatalf("%s did not parse", name)
		}
		if len(doc.Owners) != 0 {
			t.Errorf("%s: owners = %q, want none", name, doc.Owners)
		}
	}
}

// TestUnownedRoundTrip: rendering a document nobody owns and parsing it back
// must yield no owners, under both spellings of the grammar -- a pattern where
// {Names} is optional (the field is simply absent) and the shared-label
// substitution a required {Names} forces.
func TestUnownedRoundTrip(t *testing.T) {
	t.Run("optional Names", func(t *testing.T) {
		c := newConventions(t, "filename:\n  pattern: \"{DocType}_{Identifier}[_{Names}]\"\n"+
			"structure:\n  identity:\n    path: Identity\n    shared: _Shared\n")
		doc := Doc{DocType: "passport", Category: "identity", Identifier: "Home Office", Ext: ".pdf"}
		name, err := c.Render(doc)
		if err != nil {
			t.Fatal(err)
		}
		back, ok := c.Parse(name)
		if !ok {
			t.Fatalf("%s did not parse back", name)
		}
		if len(back.Owners) != 0 {
			t.Errorf("%s: owners round-tripped to %q, want none", name, back.Owners)
		}
	})

	t.Run("required Names carries the shared label", func(t *testing.T) {
		c := newConventions(t, "structure:\n  identity:\n    path: Identity\n    shared: _Shared\n")
		// The default pattern makes {Names} required, so an unowned document
		// cannot render at all; the label stands in for the missing name, the
		// way ingest substitutes it.
		doc := Doc{DocType: "passport", Category: "identity", Identifier: "Home Office", Year: 2026, Ext: ".pdf"}
		if _, err := c.Render(doc); err == nil {
			t.Fatal("expected the required {Names} field to refuse an unowned document")
		}
		doc.Owners = []string{"_Shared"}
		name, err := c.Render(doc)
		if err != nil {
			t.Fatal(err)
		}
		back, ok := c.Parse(name)
		if !ok {
			t.Fatalf("%s did not parse back", name)
		}
		if len(back.Owners) != 0 {
			t.Errorf("%s: owners round-tripped to %q, want none", name, back.Owners)
		}
		if back.OwnersAmbiguous {
			t.Error("a name Kagaz rendered itself must not read back as ambiguous")
		}
	})
}
