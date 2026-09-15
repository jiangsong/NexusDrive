package textract

import (
	"bytes"
	"fmt"
)

// PDF fixtures are built in memory so the repository carries no binary
// test data. buildPDF assembles a classic (non-object-stream) PDF with one
// page per content stream, all pages sharing one font resource, and
// computes the xref offsets itself, which is exactly what the parser
// checks first.

// helveticaFont is a standard Type1 font with a byte encoding the library
// knows, so (string) operands decode to readable text.
const helveticaFont = "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>"

// cidFontWithoutToUnicode mimics an embedded CID font that ships no
// ToUnicode map: the library has nothing to decode the codes with.
const cidFontWithoutToUnicode = "<< /Type /Font /Subtype /Type0 /BaseFont /ABCDEF+Unknown /Encoding /Identity-H >>"

// textStream shows text on one line with the page font.
func textStream(text string) string {
	return fmt.Sprintf("BT /F1 12 Tf 72 720 Td (%s) Tj ET", text)
}

// buildPDF returns a PDF with len(streams) pages whose content streams are
// given verbatim (an empty stream is a blank page). The font dictionary
// font is bound to /F1 on every page.
func buildPDF(font string, streams ...string) []byte {
	var b bytes.Buffer
	var offsets []int
	obj := func(body string) {
		offsets = append(offsets, b.Len())
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", len(offsets), body)
	}
	b.WriteString("%PDF-1.4\n")
	// Object numbers are fixed up front: 1 catalog, 2 pages, 3 font, then
	// a page and a content stream per page.
	const catalog, pages, font1 = 1, 2, 3
	var kids []byte
	for i := range streams {
		kids = fmt.Appendf(kids, "%d 0 R ", font1+1+2*i)
	}
	obj(fmt.Sprintf("<< /Type /Catalog /Pages %d 0 R >>", pages))
	obj(fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", bytes.TrimSpace(kids), len(streams)))
	obj(font)
	for _, stream := range streams {
		contents := len(offsets) + 2
		obj(fmt.Sprintf("<< /Type /Page /Parent %d 0 R /MediaBox [0 0 612 792] /Contents %d 0 R /Resources << /Font << /F1 %d 0 R >> >> >>", pages, contents, font1))
		obj(fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream))
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(offsets)+1)
	for _, o := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", o)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root %d 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets)+1, catalog, xref)
	return b.Bytes()
}

// minimalPDF builds a one-page PDF whose content stream shows text with a
// standard Type1 font.
func minimalPDF(text string) []byte {
	return buildPDF(helveticaFont, textStream(text))
}

// brokenXrefPDF returns a PDF whose cross-reference entries for the page
// tree and the font point at each other's objects. The trailer parses, so
// the failure only surfaces when the page tree is resolved, which the
// library reports with a panic rather than an error.
func brokenXrefPDF() []byte {
	good := minimalPDF("x")
	i := bytes.LastIndex(good, []byte("xref\n0 "))
	i += bytes.IndexByte(good[i:], '\n') + 1 // start of the entries
	const entry = 20                         // "nnnnnnnnnn ggggg n \n"
	out := append([]byte{}, good...)
	copy(out[i+2*entry:], good[i+3*entry:i+4*entry])
	copy(out[i+3*entry:], good[i+2*entry:i+3*entry])
	return out
}

// danglingPagesPDF returns a PDF whose page tree references an object that
// is missing from the cross-reference table.
func danglingPagesPDF() []byte {
	good := minimalPDF("x")
	return bytes.Replace(good, []byte("/Pages 2 0 R"), []byte("/Pages 9 0 R"), 1)
}
