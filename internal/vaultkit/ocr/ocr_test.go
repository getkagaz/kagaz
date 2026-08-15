package ocr

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
)

// stubLookPath replaces tool discovery for the duration of a test.
func stubLookPath(t *testing.T, found map[string]string) {
	t.Helper()
	orig := lookPath
	lookPath = func(name string) (string, error) {
		if p, ok := found[name]; ok {
			return p, nil
		}
		return "", exec.ErrNotFound
	}
	t.Cleanup(func() { lookPath = orig })
}

// prose builds n space-separated five-letter words.
func prose(n int) string {
	return strings.TrimSpace(strings.Repeat("lorem ipsum dolor sitam ", n/4))
}

func TestHasUsableTextLayer(t *testing.T) {
	tests := []struct {
		name  string
		text  string
		pages int
		want  bool
	}{
		{"empty", "", 1, false},
		{"whitespace only", "   \n\t\n ", 1, false},
		{"real text layer", prose(60), 1, true},
		{"real text layer multi page", prose(240), 2, true},
		{"thin scan layer", "Page 1 of 3\n\fPage 2 of 3\n\fPage 3 of 3", 3, false},
		{"short", "Invoice 2024-117", 1, false},
		{"mojibake", strings.Repeat("� ¶ † ¤ ‹ ", 60), 1, false},
		{"glyph soup single chars", strings.Repeat("a b c d e f ", 40), 1, false},
		{"good text but too few pages of substance", prose(60), 10, false},
		{"pages zero treated as one", prose(60), 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := HasUsableTextLayer(tc.text, tc.pages); got != tc.want {
				t.Fatalf("HasUsableTextLayer(%q, %d) = %v, want %v",
					truncate(tc.text), tc.pages, got, tc.want)
			}
		})
	}
}

func truncate(s string) string {
	if len(s) > 60 {
		return s[:60] + "..."
	}
	return s
}

func TestPDFToTextUnavailableDegradesGracefully(t *testing.T) {
	stubLookPath(t, nil)

	p := &PDFToText{}
	if p.Available() {
		t.Fatal("Available() = true with no pdftotext on PATH")
	}
	if d := p.detail(); !strings.Contains(d, "not found") {
		t.Fatalf("detail() = %q, want a not-found explanation", d)
	}
	if _, err := p.Extract(context.Background(), "/nonexistent.pdf"); !errors.Is(err, ErrNoPDFToText) {
		t.Fatalf("Extract() error = %v, want ErrNoPDFToText", err)
	}
}

func TestParsePDFInfoPages(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want int
	}{
		{"typical", "Title:          Invoice\nPages:          7\nEncrypted:      no\n", 7},
		{"single page", "Pages:  1\n", 1},
		{"no pages line", "Title: Invoice\n", 0},
		{"garbage value", "Pages: many\n", 0},
		{"zero", "Pages: 0\n", 0},
		{"empty", "", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := parsePDFInfoPages(tc.out); got != tc.want {
				t.Fatalf("parsePDFInfoPages() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestPageCountFromFormFeeds(t *testing.T) {
	tests := []struct {
		name string
		text string
		want int
	}{
		{"empty", "", 0},
		{"whitespace", "  \n ", 0},
		{"one page no form feed", "hello", 1},
		{"one page trailing form feed", "hello\f", 1},
		{"three pages", "a\fb\fc\f", 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := pageCountFromFormFeeds(tc.text); got != tc.want {
				t.Fatalf("pageCountFromFormFeeds(%q) = %d, want %d", tc.text, got, tc.want)
			}
		})
	}
}
