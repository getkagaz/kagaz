package config

import "testing"

// TestDefaultStructureGivesEverySharedLabel pins the fix for the defect that a
// vault built from the defaults could not file a single unowned document:
// only `identity` carried a shared label, so conventions.Render's deliberate
// refusal to invent an owner fired for every other category — including
// `company`, where third-party documents (a client's certificate, an
// incorporation document) overwhelmingly land.
func TestDefaultStructureGivesEverySharedLabel(t *testing.T) {
	for name, cat := range DefaultStructure() {
		if cat.Shared == "" {
			t.Errorf("structure.%s has no shared label; an unowned document in this category cannot be filed", name)
		}
		if cat.Shared != DefaultSharedFolder {
			t.Errorf("structure.%s.shared = %q, want %q", name, cat.Shared, DefaultSharedFolder)
		}
		if cat.Path == "" {
			t.Errorf("structure.%s has no path", name)
		}
		if cat.Layout == "" {
			t.Errorf("structure.%s has no layout", name)
		}
	}
}

// TestDefaultCategoriesMatchesDefaultStructure keeps the rendering order used
// by `kagaz init` in step with the map it renders: a category present in one
// and not the other would be written into a user's vault.yaml incompletely or
// not at all.
func TestDefaultCategoriesMatchesDefaultStructure(t *testing.T) {
	s := DefaultStructure()
	names := DefaultCategories()
	if len(names) != len(s) {
		t.Fatalf("DefaultCategories has %d names, DefaultStructure has %d categories", len(names), len(s))
	}
	seen := map[string]bool{}
	for _, n := range names {
		if seen[n] {
			t.Errorf("DefaultCategories lists %q twice", n)
		}
		seen[n] = true
		if _, ok := s[n]; !ok {
			t.Errorf("DefaultCategories lists %q, which DefaultStructure does not define", n)
		}
	}
	for n := range s {
		if !seen[n] {
			t.Errorf("DefaultStructure defines %q, which DefaultCategories omits", n)
		}
	}
}

// TestMinimalConfigDefaultsToASharedLabelEverywhere covers the path a real
// vault takes: a vault.yaml with no `structure:` block at all must still come
// out of Parse with a shared label on every category.
func TestMinimalConfigDefaultsToASharedLabelEverywhere(t *testing.T) {
	cfg, err := Parse([]byte("version: 1\nvault_root: .\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.Structure) == 0 {
		t.Fatal("no structure was defaulted")
	}
	for name, cat := range cfg.Structure {
		if cat.Shared == "" {
			t.Errorf("structure.%s.shared is empty after defaulting", name)
		}
	}
}

// TestExplicitStructureKeepsAnEmptySharedLabel guards the other half of the
// contract: defaulting fills in a shared label only when the vault supplies no
// structure at all. A vault that names a category and deliberately leaves
// `shared` off wants Render's refusal, and must keep it.
func TestExplicitStructureKeepsAnEmptySharedLabel(t *testing.T) {
	cfg, err := Parse([]byte("version: 1\nvault_root: .\nstructure:\n  company:\n    path: Company\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := cfg.Structure["company"].Shared; got != "" {
		t.Fatalf("an explicitly declared category gained shared=%q; the refusal path is no longer reachable", got)
	}
}
