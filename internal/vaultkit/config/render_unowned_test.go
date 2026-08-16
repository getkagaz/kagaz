// This file is an external test package on purpose: conventions imports
// config, so exercising Render from inside package config would be an import
// cycle.
package config_test

import (
	"strings"
	"testing"

	"github.com/getkagaz/kagaz/internal/vaultkit/config"
	"github.com/getkagaz/kagaz/internal/vaultkit/conventions"
)

// TestFreshConfigRendersAnUnownedDocument is the regression test for the
// defect found by running Kagaz against real paperwork: a vault created from
// the defaults skipped every third-party document with "category %q defines no
// shared folder". Render's refusal is correct and stays; what was missing was
// a shared label in the default config for it to use.
func TestFreshConfigRendersAnUnownedDocument(t *testing.T) {
	cfg, err := config.Parse([]byte("version: 1\nvault_root: .\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	conv, err := conventions.New(cfg)
	if err != nil {
		t.Fatalf("conventions.New: %v", err)
	}

	for name := range cfg.Structure {
		doc := conventions.Doc{
			DocType:    "certificate",
			Category:   name,
			Identifier: "Northwind Holdings",
			Year:       2026,
			Ext:        ".pdf",
		}
		got, err := conv.Render(doc)
		if err != nil {
			t.Errorf("category %q: unowned document does not render: %v", name, err)
			continue
		}
		if !strings.Contains(got, conv.Word(config.DefaultSharedFolder)) {
			t.Errorf("category %q: rendered %q, which does not carry the shared label", name, got)
		}
		if _, err := conv.Path(doc); err != nil {
			t.Errorf("category %q: unowned document has no path: %v", name, err)
		}
	}
}
