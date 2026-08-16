package cli

import (
	"bytes"
	"fmt"
	"strings"
)

// renderPDF writes a one-page, uncompressed PDF containing title and body as
// Helvetica text.
//
// It exists so that `kagaz init --demo` produces documents that are really
// PDFs — openable in Preview, readable by pdftotext, indexable by Spotlight —
// rather than text files with a .pdf extension, which would make the demo
// vault a worse rehearsal of the real thing than it needs to be. The output is
// deliberately the simplest legal structure: no compression, no font
// embedding, five objects and a hand-built xref table.
func renderPDF(title string, body []string) []byte {
	const (
		pageWidth  = 612
		pageHeight = 792
		leading    = 14
		fontSize   = 11
		leftMargin = 64
		topMargin  = 84
	)

	var content bytes.Buffer
	content.WriteString("BT\n")
	fmt.Fprintf(&content, "/F1 %d Tf\n", fontSize+3)
	fmt.Fprintf(&content, "%d %d Td\n", leftMargin, pageHeight-topMargin)
	fmt.Fprintf(&content, "%d TL\n", leading+6)
	fmt.Fprintf(&content, "(%s) Tj\n", escapePDFText(title))
	content.WriteString("T*\n")
	fmt.Fprintf(&content, "/F1 %d Tf\n", fontSize)
	fmt.Fprintf(&content, "%d TL\n", leading)
	content.WriteString("T*\n")
	for _, line := range body {
		fmt.Fprintf(&content, "(%s) Tj\n", escapePDFText(line))
		content.WriteString("T*\n")
	}
	content.WriteString("ET\n")

	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %d %d] "+
			"/Resources << /Font << /F1 5 0 R >> >> /Contents 4 0 R >>", pageWidth, pageHeight),
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", content.Len(), content.String()),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>",
	}

	var out bytes.Buffer
	out.WriteString("%PDF-1.4\n")
	// A binary comment marks the file as containing binary data, which is what
	// every PDF writer emits and what some readers sniff for.
	out.Write([]byte{'%', 0xE2, 0xE3, 0xCF, 0xD3, '\n'})

	offsets := make([]int, len(objects)+1)
	for i, obj := range objects {
		offsets[i+1] = out.Len()
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", i+1, obj)
	}

	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n", len(objects)+1)
	out.WriteString("0000000000 65535 f \n")
	for i := 1; i <= len(objects); i++ {
		fmt.Fprintf(&out, "%010d 00000 n \n", offsets[i])
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n",
		len(objects)+1, xref)
	return out.Bytes()
}

// escapePDFText escapes the three characters that are syntax inside a PDF
// literal string, and drops anything outside Latin-1, which the WinAnsi
// encoding above cannot represent.
func escapePDFText(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\\' || r == '(' || r == ')':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteByte(' ')
		case r < 32:
			// Control characters have no visible rendering; drop them.
		case r < 127:
			b.WriteRune(r)
		case r == '—' || r == '–':
			b.WriteString("-")
		case r <= 0xFF:
			b.WriteByte(byte(r))
		default:
			b.WriteByte('?')
		}
	}
	return b.String()
}
