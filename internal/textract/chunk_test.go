package textract

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestChunksNeverSplitARuneAndMatchTheirOffsets(t *testing.T) {
	d := Doc{Text: strings.Repeat("云端文件系统。", 700), OffsetKind: "file"}
	chunks := ChunkDoc(d, KindText, DefaultChunkOptions())
	if len(chunks) < 5 {
		t.Fatalf("%d chunks", len(chunks))
	}
	for i, c := range chunks {
		if !utf8.ValidString(c.Text) || c.Text != d.Text[c.StartOff:c.EndOff] {
			t.Fatalf("chunk %d offsets do not match its text", i)
		}
		if n := utf8.RuneCountInString(c.Text); n > 800 {
			t.Fatalf("chunk %d has %d runes", i, n)
		}
		if c.Seq != i {
			t.Fatalf("seq %d at %d", c.Seq, i)
		}
	}
}

func TestConsecutiveChunksOverlapByAboutAHundredRunes(t *testing.T) {
	d := Doc{Text: strings.Repeat("a", 3000)}
	c := ChunkDoc(d, KindText, DefaultChunkOptions())
	if len(c) < 4 {
		t.Fatalf("%d chunks", len(c))
	}
	for i := 1; i < len(c); i++ {
		overlap := c[i-1].EndOff - c[i].StartOff
		if overlap < 80 || overlap > 120 {
			t.Fatalf("overlap %d between %d and %d", overlap, i-1, i)
		}
	}
}

func TestSentenceBoundariesArePreferred(t *testing.T) {
	d := Doc{Text: strings.Repeat("短句。", 200) + strings.Repeat("长", 900)}
	c := ChunkDoc(d, KindText, DefaultChunkOptions())
	if !strings.HasSuffix(c[0].Text, "。") {
		t.Fatalf("first chunk ends mid-sentence: %q", c[0].Text[len(c[0].Text)-9:])
	}
}

func TestParagraphBreaksBeatSentenceEndsAndLineEndsBeatSpaces(t *testing.T) {
	// The paragraph break is the last strong boundary in the second half of
	// the window; the spaces after it must not pull the cut forward.
	para := strings.Repeat("word. ", 90) + "\n\n" + strings.Repeat("tail ", 20) + strings.Repeat("x", 800)
	c := ChunkDoc(Doc{Text: para}, KindText, DefaultChunkOptions())
	if !strings.HasSuffix(c[0].Text, "\n\n") {
		t.Fatalf("first chunk does not end at the paragraph break: %q", c[0].Text[len(c[0].Text)-12:])
	}
	// Without a sentence end, a line end wins over the spaces after it.
	lines := strings.Repeat("lorem ipsum ", 45) + "\n" + strings.Repeat("dolor sit ", 20) + strings.Repeat("y", 800)
	c = ChunkDoc(Doc{Text: lines}, KindText, DefaultChunkOptions())
	if !strings.HasSuffix(c[0].Text, "\n") {
		t.Fatalf("first chunk does not end at the line end: %q", c[0].Text[len(c[0].Text)-12:])
	}
	// With neither, the last space in the second half of the window is used.
	spaces := strings.Repeat("alpha beta ", 100)
	c = ChunkDoc(Doc{Text: spaces}, KindText, DefaultChunkOptions())
	if !strings.HasSuffix(c[0].Text, " ") || utf8.RuneCountInString(c[0].Text) < 400 {
		t.Fatalf("first chunk does not end at a space: %q", c[0].Text[len(c[0].Text)-12:])
	}
}

func TestASCIIPeriodsInsideTokensAreNotSentenceEnds(t *testing.T) {
	// The only periods in the second half of the window sit inside tokens,
	// so the window must fall back to the space boundary instead.
	src := strings.Repeat("v1.2.3 ", 200)
	c := ChunkDoc(Doc{Text: src}, KindText, DefaultChunkOptions())
	if strings.HasSuffix(c[0].Text, ".") {
		t.Fatalf("chunk was cut inside a version string: %q", c[0].Text[len(c[0].Text)-8:])
	}
}

func TestHeadingPathFollowsSections(t *testing.T) {
	src := "# 第二章\n" + strings.Repeat("x", 50) + "\n## 2.1 范围\n" + strings.Repeat("y", 50) + "\n"
	d, _ := Extract(context.Background(), KindMarkdown, strings.NewReader(src), int64(len(src)), DefaultOptions())
	c := ChunkDoc(d, KindMarkdown, DefaultChunkOptions())
	if len(c) != 2 || c[0].Heading != "第二章" || c[1].Heading != "第二章 > 2.1 范围" {
		t.Fatalf("%+v", c)
	}
}

func TestHeadingStackPopsSiblingsAndDeeperLevels(t *testing.T) {
	src := "intro\n# A\na\n## A.1\na1\n### A.1.x\nax\n## A.2\na2\n# B\nb\n"
	d, err := extractString(t, KindMarkdown, src, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	c := ChunkDoc(d, KindMarkdown, DefaultChunkOptions())
	want := []string{"", "A", "A > A.1", "A > A.1 > A.1.x", "A > A.2", "B"}
	var got []string
	for _, ch := range c {
		got = append(got, ch.Heading)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("headings %q, want %q", got, want)
	}
	if c[0].Text != "intro\n" || c[1].Text != "# A\na\n" || c[5].Text != "# B\nb\n" {
		t.Fatalf("%+v", c)
	}
}

func TestNoChunkStraddlesAHeadingInsideItsWindow(t *testing.T) {
	// A short first section and a heading 300 runes in: the 800-rune window
	// would swallow the heading, so the chunk must stop before it.
	src := strings.Repeat("p", 300) + "\n# Title\n" + strings.Repeat("q", 900) + "\n"
	d, err := extractString(t, KindMarkdown, src, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	c := ChunkDoc(d, KindMarkdown, DefaultChunkOptions())
	if c[0].Heading != "" || c[0].EndOff != int64(d.Headings[0].Offset) {
		t.Fatalf("first chunk %+v, heading at %d", c[0], d.Headings[0].Offset)
	}
	for i, ch := range c[1:] {
		if ch.Heading != "Title" {
			t.Fatalf("chunk %d under %q", i+1, ch.Heading)
		}
		if ch.StartOff < int64(d.Headings[0].Offset) {
			t.Fatalf("chunk %d starts before its heading", i+1)
		}
	}
	if !strings.HasPrefix(c[1].Text, "# Title\n") {
		t.Fatalf("section does not start at its heading line: %q", c[1].Text[:16])
	}
}

func TestMarkdownChunkOffsetsAreFileOffsets(t *testing.T) {
	src := "# 前言\n" + strings.Repeat("第一段内容。", 60) + "\r\n\r\n## 第二节\n" + strings.Repeat("第二段内容。", 200) + "\n"
	d, err := extractString(t, KindMarkdown, src, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if d.OffsetKind != "file" {
		t.Fatalf("offset kind %q", d.OffsetKind)
	}
	c := ChunkDoc(d, KindMarkdown, DefaultChunkOptions())
	if len(c) < 3 {
		t.Fatalf("%d chunks", len(c))
	}
	for i, ch := range c {
		// What read_text(offset) would serve from the file must start with
		// the chunk's own text.
		if !strings.HasPrefix(src[ch.StartOff:], ch.Text) {
			t.Fatalf("chunk %d at %d is not a prefix of the file from there", i, ch.StartOff)
		}
	}
}

func TestCodeUsesTheWiderWindowAndOnlyBlankLines(t *testing.T) {
	// 40-rune lines: sentence-looking periods everywhere, one blank line at
	// rune 1000, nothing else.
	line := "x = call(a.b, c.d); // done. ok? yes!\n" // 39 bytes
	src := strings.Repeat(line, 25) + "\n" + strings.Repeat(line, 40)
	c := ChunkDoc(Doc{Text: src}, KindCode, DefaultChunkOptions())
	if n := utf8.RuneCountInString(c[0].Text); n <= 800 || n > 1200 {
		t.Fatalf("code chunk has %d runes, want 800 < n <= 1200", n)
	}
	if !strings.HasSuffix(c[0].Text, "\n\n") {
		t.Fatalf("code chunk does not end at the blank line: %q", c[0].Text[len(c[0].Text)-12:])
	}
	// The same text as prose is cut at the default window and at a period.
	p := ChunkDoc(Doc{Text: src}, KindText, DefaultChunkOptions())
	if n := utf8.RuneCountInString(p[0].Text); n > 800 {
		t.Fatalf("prose chunk has %d runes", n)
	}
}

func TestAVeryLongSingleLineStillChunks(t *testing.T) {
	d := Doc{Text: strings.Repeat("字", 5000)}
	c := ChunkDoc(d, KindText, DefaultChunkOptions())
	if len(c) < 6 {
		t.Fatalf("%d chunks", len(c))
	}
	for i, ch := range c {
		if n := utf8.RuneCountInString(ch.Text); n > 800 || !utf8.ValidString(ch.Text) {
			t.Fatalf("chunk %d: %d runes valid=%v", i, n, utf8.ValidString(ch.Text))
		}
	}
	if last := c[len(c)-1]; last.EndOff != int64(len(d.Text)) {
		t.Fatalf("last chunk ends at %d, text is %d bytes", last.EndOff, len(d.Text))
	}
}

func TestChunkingIsDeterministic(t *testing.T) {
	d := Doc{Text: strings.Repeat("abc def。", 400)}
	a, b := ChunkDoc(d, KindText, DefaultChunkOptions()), ChunkDoc(d, KindText, DefaultChunkOptions())
	if !reflect.DeepEqual(a, b) {
		t.Fatal("chunking is not deterministic")
	}
}

func TestEmptyDocHasNoChunks(t *testing.T) {
	if n := len(ChunkDoc(Doc{}, KindText, DefaultChunkOptions())); n != 0 {
		t.Fatal(n)
	}
	if n := len(ChunkDoc(Doc{Text: " \n\t\n"}, KindText, DefaultChunkOptions())); n != 0 {
		t.Fatal(n)
	}
}

func TestShortSectionIsASingleChunk(t *testing.T) {
	d := Doc{Text: "just a few words.\n"}
	c := ChunkDoc(d, KindText, DefaultChunkOptions())
	if len(c) != 1 || c[0].StartOff != 0 || c[0].EndOff != int64(len(d.Text)) || c[0].Seq != 0 {
		t.Fatalf("%+v", c)
	}
}

func TestZeroAndOversizedOptionsStillTerminate(t *testing.T) {
	d := Doc{Text: strings.Repeat("z", 2500)}
	zero := ChunkDoc(d, KindText, ChunkOptions{})
	def := ChunkDoc(d, KindText, DefaultChunkOptions())
	if !reflect.DeepEqual(zero, def) {
		t.Fatal("zero options do not equal the defaults")
	}
	// An overlap at or past the window must not stall the walk.
	c := ChunkDoc(d, KindText, ChunkOptions{Window: 100, Overlap: 100})
	if len(c) == 0 || c[len(c)-1].EndOff != int64(len(d.Text)) {
		t.Fatalf("%d chunks, last %+v", len(c), c[len(c)-1])
	}
	for i := 1; i < len(c); i++ {
		if c[i].StartOff <= c[i-1].StartOff {
			t.Fatalf("chunk %d does not advance", i)
		}
	}
}

func TestOutOfRangeHeadingsAreIgnored(t *testing.T) {
	d := Doc{
		Text: "# A\nbody\n",
		Headings: []Heading{
			{Level: 1, Title: "ghost", Offset: 99},
			{Level: 1, Title: "neg", Offset: -1},
			{Level: 1, Title: "A", Offset: 0},
		},
		OffsetKind: "text",
	}
	c := ChunkDoc(d, KindText, DefaultChunkOptions())
	if len(c) != 1 || c[0].Heading != "A" || c[0].Text != d.Text {
		t.Fatalf("%+v", c)
	}
}
