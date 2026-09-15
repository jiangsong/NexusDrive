package textract

// PDF extraction on top of github.com/ledongthuc/pdf, the one third-party
// dependency of this package. The library is a text extractor, not a
// renderer: it walks the content streams of every page, decodes the shown
// strings through the page fonts and has no OCR, so scanned documents come
// out as ErrNoText. It also reports malformed files by panicking from deep
// inside its object resolver, which is why the parser runs on its own
// goroutine behind a recover and a context watch (DESIGN.md §9, T-37).
//
// UNVERIFIED: Chinese PDF extraction quality. CJK PDFs (CID fonts) often
// extract as mojibake with this library; verify quality on real Chinese
// documents before promising it. The garbled heuristic below turns the
// worst cases into ErrGarbled rather than indexing noise.

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode"

	"github.com/ledongthuc/pdf"
)

// garbledPercent is the share of bad runes past which extracted text is
// considered unreadable.
const garbledPercent = 30

// extractPDF emits "## Page N" followed by one line per text line for every
// page that carries text, in page order. The library runs on a separate
// goroutine so that a panic inside it becomes an error and so that the
// context deadline derived from Options.Timeout is honoured even while the
// parser is stuck in a slow ReaderAt. After a timeout that goroutine is
// abandoned and may run on until it finishes with the ReaderAt it holds;
// its memory is bounded by the file size (the indexer's max_file_size)
// because the library keeps at most one page's content in memory, and the
// text kept for the Doc is bounded by MaxTextBytes.
func extractPDF(ctx context.Context, r io.ReaderAt, size int64, opt Options) (Doc, error) {
	type result struct {
		doc Doc
		err error
	}
	done := make(chan result, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				done <- result{err: fmt.Errorf("textract: pdf parser panicked: %v", p)}
			}
		}()
		doc, err := pdfPages(ctx, r, size, opt)
		done <- result{doc: doc, err: err}
	}()
	select {
	case <-ctx.Done():
		return Doc{}, ctx.Err()
	case res := <-done:
		return res.doc, res.err
	}
}

// pdfPages is the body of extractPDF: it opens the file, walks the pages
// in order and builds the Doc. It polls ctx between pages, so a slow
// document stops within one page of the deadline even when the caller's
// select has already returned.
func pdfPages(ctx context.Context, r io.ReaderAt, size int64, opt Options) (Doc, error) {
	rd, err := pdf.NewReader(r, size)
	if err != nil {
		return Doc{}, fmt.Errorf("textract: pdf: %w", err)
	}
	b := newDocBuilder(opt)
	fonts := map[string]*pdf.Font{}
	var bad, total int
	for i, n := 1, rd.NumPage(); i <= n && !b.full(); i++ {
		if err := ctx.Err(); err != nil {
			return Doc{}, err
		}
		p := rd.Page(i)
		if p.V.IsNull() {
			continue
		}
		// Fonts are cached across pages so a ToUnicode cmap is parsed once.
		for _, name := range p.Fonts() {
			if _, ok := fonts[name]; !ok {
				f := p.Font(name)
				fonts[name] = &f
			}
		}
		text, err := p.GetPlainText(fonts)
		if err != nil {
			return Doc{}, fmt.Errorf("textract: pdf page %d: %w", i, err)
		}
		lines := pdfLines(text)
		if len(lines) == 0 {
			continue
		}
		b.heading(2, "Page "+strconv.Itoa(i))
		for _, line := range lines {
			if b.full() {
				break
			}
			pb, pt := garbledCount(line)
			bad, total = bad+pb, total+pt
			b.line(line)
		}
	}
	doc, err := b.doc()
	if err != nil {
		return Doc{}, err
	}
	if garbledRatio(bad, total) {
		return Doc{}, ErrGarbled
	}
	return doc, nil
}

// pdfLines splits the text the library produced for a page into trimmed,
// non-blank lines of valid UTF-8. The library only breaks lines at the
// T*, ' and " operators and hands raw bytes through for fonts it cannot
// decode, so invalid sequences become U+FFFD, which the garbled heuristic
// then counts.
func pdfLines(text string) []string {
	text = strings.ToValidUTF8(text, "\uFFFD")
	var out []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// garbled reports whether more than garbledPercent of the runes in s are
// U+FFFD, C0/C1 controls other than \t \n \r, or private-use characters:
// the signature of a font whose codes could not be mapped to Unicode.
func garbled(s string) bool {
	return garbledRatio(garbledCount(s))
}

// garbledRatio applies the threshold to a tally from garbledCount.
func garbledRatio(bad, total int) bool {
	return total > 0 && bad*100 > total*garbledPercent
}

// garbledCount tallies the bad runes in s as garbled defines them, together
// with the total rune count, so callers can accumulate over several
// strings. Invalid UTF-8 bytes decode as U+FFFD and count as bad.
func garbledCount(s string) (bad, total int) {
	for _, r := range s {
		total++
		switch {
		case r == unicode.ReplacementChar:
			bad++
		case r == '\t' || r == '\n' || r == '\r':
		case r < 0x20 || (r >= 0x7f && r < 0xa0):
			bad++
		case unicode.Is(unicode.Co, r):
			bad++
		}
	}
	return bad, total
}
