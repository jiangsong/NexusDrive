package textract

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestMinimalPDFExtractsText(t *testing.T) {
	d, err := extractBytes(t, KindPDF, minimalPDF("Quarterly cloud storage report"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d.Text, "Quarterly cloud storage report") || d.OffsetKind != "text" {
		t.Fatalf("%q", d.Text)
	}
	if d.Truncated {
		t.Fatal("small document marked truncated")
	}
}

func TestPDFPagesBecomeHeadingsInPageOrder(t *testing.T) {
	d, err := extractBytes(t, KindPDF, buildPDF(helveticaFont,
		textStream("first page"), textStream("second page"), textStream("third page")))
	if err != nil {
		t.Fatal(err)
	}
	want := "## Page 1\nfirst page\n## Page 2\nsecond page\n## Page 3\nthird page\n"
	if d.Text != want {
		t.Fatalf("text %q want %q", d.Text, want)
	}
	if len(d.Headings) != 3 {
		t.Fatalf("headings %+v", d.Headings)
	}
	for i, h := range d.Headings {
		if h.Level != 2 || h.Title != "Page "+string(rune('1'+i)) {
			t.Fatalf("heading %d: %+v", i, h)
		}
		if !strings.HasPrefix(d.Text[h.Offset:], "## "+h.Title+"\n") {
			t.Fatalf("heading %d offset %d points at %q", i, h.Offset, d.Text[h.Offset:])
		}
	}
}

func TestPDFBlankPagesGetNoHeading(t *testing.T) {
	d, err := extractBytes(t, KindPDF, buildPDF(helveticaFont,
		"", textStream("only page with words"), "0 0 m 100 100 l S"))
	if err != nil {
		t.Fatal(err)
	}
	if d.Text != "## Page 2\nonly page with words\n" {
		t.Fatalf("%q", d.Text)
	}
}

func TestPDFWithoutTextIsNoText(t *testing.T) {
	for name, b := range map[string][]byte{
		"blank page":   buildPDF(helveticaFont, ""),
		"drawing only": buildPDF(helveticaFont, "0 0 m 100 100 l S"),
		"no pages":     buildPDF(helveticaFont),
	} {
		if _, err := extractBytes(t, KindPDF, b); !errors.Is(err, ErrNoText) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestPDFTextLinesAreSplitAndTrimmed(t *testing.T) {
	d, err := extractBytes(t, KindPDF, buildPDF(helveticaFont,
		"BT /F1 12 Tf 72 720 Td 14 TL (  alpha  ) Tj T* (beta) Tj T* (   ) Tj T* T* (gamma) Tj ET"))
	if err != nil {
		t.Fatal(err)
	}
	if d.Text != "## Page 1\nalpha\nbeta\ngamma\n" {
		t.Fatalf("%q", d.Text)
	}
}

func TestMalformedPDFDoesNotPanic(t *testing.T) {
	good := minimalPDF("x")
	for name, b := range map[string][]byte{
		"header only":    []byte("%PDF-1.4\ngarbage"),
		"cut in half":    good[:len(good)/2],
		"binary noise":   bytes.Repeat([]byte{0xff}, 4096),
		"broken xref":    brokenXrefPDF(),
		"dangling pages": danglingPagesPDF(),
		"empty":          nil,
	} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("%s: panic escaped: %v", name, r)
				}
			}()
			_, err := extractBytes(t, KindPDF, b)
			if err == nil {
				t.Fatalf("%s: malformed PDF extracted without error", name)
			}
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				t.Fatalf("%s: reported as a context error: %v", name, err)
			}
		}()
	}
}

func TestBrokenXrefIsRecoveredFromAParserPanic(t *testing.T) {
	// The library reports this one by panicking; the error must say so
	// rather than crash the caller.
	_, err := extractBytes(t, KindPDF, brokenXrefPDF())
	if err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Fatalf("expected a recovered panic, got %v", err)
	}
}

func TestGarbledTextIsDetected(t *testing.T) {
	if garbled("正常的中文文本和 English words") {
		t.Fatal("clean text flagged")
	}
	if garbled("tabs\tand\r\nnewlines are fine") {
		t.Fatal("whitespace controls flagged")
	}
	if !garbled(strings.Repeat("\uFFFD", 40) + "ok") {
		t.Fatal("replacement characters not flagged")
	}
	if !garbled(strings.Repeat("\x01\x02", 30) + "ok") {
		t.Fatal("control characters not flagged")
	}
	if !garbled(strings.Repeat("\uE000\uF8FF", 20) + "ok") {
		t.Fatal("private-use characters not flagged")
	}
	if !garbled("caf\xe9 \xff\xfe\xfd") {
		t.Fatal("invalid UTF-8 not flagged")
	}
	if garbled("") {
		t.Fatal("empty string flagged")
	}
	// Exactly 30% bad is tolerated; the threshold is strictly greater.
	if garbled(strings.Repeat("\x01", 3) + strings.Repeat("a", 7)) {
		t.Fatal("30% flagged")
	}
	if !garbled(strings.Repeat("\x01", 4) + strings.Repeat("a", 6)) {
		t.Fatal("40% not flagged")
	}
}

func TestPDFWithUndecodableFontIsGarbled(t *testing.T) {
	// A CID font without a ToUnicode map decodes 2-byte codes through the
	// PDFDoc byte table, where the low bytes have no character.
	stream := "BT /F1 12 Tf 72 720 Td <00010002000300040005000600070008> Tj ET"
	_, err := extractBytes(t, KindPDF, buildPDF(cidFontWithoutToUnicode, stream))
	if !errors.Is(err, ErrGarbled) {
		t.Fatalf("expected ErrGarbled, got %v", err)
	}
}

func TestPDFGarbleIsJudgedOnTheWholeDocument(t *testing.T) {
	// One unreadable line among many readable ones does not condemn the
	// document, and the "## Page N" headings do not dilute the ratio.
	d, err := extractBytes(t, KindPDF, buildPDF(helveticaFont,
		textStream("readable words on the first page"),
		"BT /F1 12 Tf 72 720 Td <0102> Tj ET",
		textStream("and more readable words afterwards")))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d.Text, "## Page 2\n") {
		t.Fatalf("%q", d.Text)
	}
	_, err = extractBytes(t, KindPDF, buildPDF(helveticaFont,
		textStream("ok"),
		"BT /F1 12 Tf 72 720 Td <01020304050607> Tj ET"))
	if !errors.Is(err, ErrGarbled) {
		t.Fatalf("expected ErrGarbled, got %v", err)
	}
}

func TestPDFTextIsTruncatedAtTheLimit(t *testing.T) {
	opt := DefaultOptions()
	opt.MaxTextBytes = 40
	var streams []string
	for i := 0; i < 20; i++ {
		streams = append(streams, textStream("twenty bytes of text"))
	}
	d, err := extractBytesWith(t, KindPDF, buildPDF(helveticaFont, streams...), opt)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Truncated || int64(len(d.Text)) > opt.MaxTextBytes {
		t.Fatalf("truncated=%v len=%d %q", d.Truncated, len(d.Text), d.Text)
	}
}

type slowReaderAt struct{ b []byte }

func (s slowReaderAt) ReadAt(p []byte, off int64) (int, error) {
	time.Sleep(50 * time.Millisecond)
	return bytes.NewReader(s.b).ReadAt(p, off)
}

func TestPDFTimeoutIsHonoured(t *testing.T) {
	opt := DefaultOptions()
	opt.Timeout = 20 * time.Millisecond
	b := minimalPDF(strings.Repeat("slow ", 100))
	start := time.Now()
	_, err := Extract(context.Background(), KindPDF, slowReaderAt{b}, int64(len(b)), opt)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 2*time.Second {
		t.Fatalf("%v after %v", err, time.Since(start))
	}
}

func TestPDFCancelledContextIsReported(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	b := minimalPDF("x")
	if _, err := Extract(ctx, KindPDF, bytes.NewReader(b), int64(len(b)), DefaultOptions()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
