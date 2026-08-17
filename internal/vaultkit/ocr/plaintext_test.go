package ocr

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTemp writes content to a file named name in a fresh temp dir.
func writeTemp(t *testing.T, name string, content []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestPlainTextRefusesLargeBinary: a NUL-free binary file large enough to fill
// the read budget must still be refused. The size cap forgives a rune split by
// truncation, not a whole file of invalid UTF-8 wearing a .txt extension.
func TestPlainTextRefusesLargeBinary(t *testing.T) {
	// Stray continuation bytes (0x80) interleaved with printable ASCII: no
	// NUL anywhere, so only the UTF-8 check can catch it, and stripping the
	// bad bytes would leave plausible-looking text rather than nothing.
	blob := make([]byte, MaxPlainTextBytes+4096)
	for i := range blob {
		if i%2 == 0 {
			blob[i] = 'A'
		} else {
			blob[i] = 0x80
		}
	}
	path := writeTemp(t, "binary.txt", blob)

	res, err := (&PlainText{}).Extract(context.Background(), path)
	if err == nil {
		t.Fatalf("a 1 MiB NUL-free binary .txt was accepted as %d bytes of text", len(res.Text))
	}
	if !errors.Is(err, ErrNoText) {
		t.Fatalf("error = %v, want it to wrap ErrNoText", err)
	}
	if !strings.Contains(err.Error(), "not valid UTF-8") {
		t.Errorf("the refusal does not say why: %v", err)
	}
}

// TestPlainTextKeepsTruncatedMultibyteRune: a legitimate UTF-8 file cut mid-rune
// by the read budget is still accepted, with only the partial tail rune dropped.
func TestPlainTextKeepsTruncatedMultibyteRune(t *testing.T) {
	// "…" is 3 bytes (E2 80 A6). Lay out ASCII filler so that the very last
	// rune in the read window is split with one byte to spare.
	const filler = "kagaz text layer "
	head := strings.Repeat(filler, MaxPlainTextBytes/len(filler)+1)
	head = head[:MaxPlainTextBytes-2] // two bytes of the ellipsis fit
	content := []byte(head + "…" + strings.Repeat("x", 64))
	path := writeTemp(t, "truncated.txt", content)

	res, err := (&PlainText{}).Extract(context.Background(), path)
	if err != nil {
		t.Fatalf("a truncated but valid UTF-8 file was refused: %v", err)
	}
	if res.Text != head {
		t.Errorf("text is %d bytes, want the %d-byte head with only the split rune dropped",
			len(res.Text), len(head))
	}
	if len(res.Text) != MaxPlainTextBytes-2 {
		t.Errorf("trimmed to %d bytes; only the two-byte partial rune should have gone", len(res.Text))
	}
}
