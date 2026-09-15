package textract

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func extractString(t *testing.T, kind Kind, s string, opt Options) (Doc, error) {
	t.Helper()
	return Extract(context.Background(), kind, strings.NewReader(s), int64(len(s)), opt)
}

func TestTextKeepsCRLFSoOffsetsAreFileOffsets(t *testing.T) {
	src := "line one\r\nline two\r\n"
	d, err := extractString(t, KindText, src, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if d.Text != src || d.OffsetKind != "file" {
		t.Fatalf("%q %q", d.Text, d.OffsetKind)
	}
}

func TestNULBytesBecomeSpacesWithoutShiftingOffsets(t *testing.T) {
	d, _ := extractString(t, KindText, "a\x00b", DefaultOptions())
	if d.Text != "a b" {
		t.Fatalf("%q", d.Text)
	}
}

func TestNonUTF8Fails(t *testing.T) {
	if _, err := extractString(t, KindText, "caf\xe9", DefaultOptions()); !errors.Is(err, ErrNotUTF8) {
		t.Fatal(err)
	}
}

func TestMarkdownHeadingsCarryOffsets(t *testing.T) {
	src := "intro\n# 第一章\nbody\n## 1.1 范围\nmore\n"
	d, _ := extractString(t, KindMarkdown, src, DefaultOptions())
	if len(d.Headings) != 2 || d.Headings[0].Title != "第一章" || d.Headings[1].Level != 2 {
		t.Fatalf("%+v", d.Headings)
	}
	if !strings.HasPrefix(src[d.Headings[1].Offset:], "## 1.1 范围") {
		t.Fatalf("offset %d", d.Headings[1].Offset)
	}
}

func TestMarkdownHeadingsInsideFencedCodeAreIgnored(t *testing.T) {
	src := "# real\n```sh\n# not a heading\n```\n~~~\n## also not\n~~~\n### real too\n"
	d, err := extractString(t, KindMarkdown, src, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Headings) != 2 || d.Headings[0].Title != "real" || d.Headings[1].Title != "real too" || d.Headings[1].Level != 3 {
		t.Fatalf("%+v", d.Headings)
	}
}

func TestMarkdownHeadingOffsetsSurviveCRLF(t *testing.T) {
	src := "intro\r\n## 标题\r\nbody\r\n"
	d, _ := extractString(t, KindMarkdown, src, DefaultOptions())
	if len(d.Headings) != 1 || d.Headings[0].Title != "标题" {
		t.Fatalf("%+v", d.Headings)
	}
	if !strings.HasPrefix(src[d.Headings[0].Offset:], "## 标题\r\n") {
		t.Fatalf("offset %d", d.Headings[0].Offset)
	}
}

func TestTextIsTruncatedOnARuneBoundary(t *testing.T) {
	opt := DefaultOptions()
	opt.MaxTextBytes = 7
	d, _ := extractString(t, KindText, "中文字符串", opt) // 3 bytes per rune
	if !d.Truncated || d.Text != "中文" {
		t.Fatalf("%q %v", d.Text, d.Truncated)
	}
}

func TestTextAtExactlyTheLimitIsNotTruncated(t *testing.T) {
	opt := DefaultOptions()
	opt.MaxTextBytes = 6
	d, err := extractString(t, KindText, "中文", opt)
	if err != nil {
		t.Fatal(err)
	}
	if d.Truncated || d.Text != "中文" {
		t.Fatalf("%q %v", d.Text, d.Truncated)
	}
}

func TestCodeIsExtractedVerbatimWithoutHeadings(t *testing.T) {
	src := "package x\n\n# not markdown\nfunc main() {}\n"
	d, err := extractString(t, KindCode, src, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if d.Text != src || len(d.Headings) != 0 || d.OffsetKind != "file" {
		t.Fatalf("%q %+v %q", d.Text, d.Headings, d.OffsetKind)
	}
}

func TestEmptyTextIsNoText(t *testing.T) {
	if _, err := extractString(t, KindText, "", DefaultOptions()); !errors.Is(err, ErrNoText) {
		t.Fatal(err)
	}
}

func TestMarkdownHeadingTitlesFollowCommonMark(t *testing.T) {
	src := "# foo ##\n# foo#\n# ###\n#nospace\n####### seven\n   ### indented\n    #### code block\n#\ttabbed\n"
	d, err := extractString(t, KindMarkdown, src, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	want := []Heading{
		{Level: 1, Title: "foo", Offset: 0},
		{Level: 1, Title: "foo#", Offset: 9},
		{Level: 3, Title: "indented", Offset: int64(strings.Index(src, "   ###"))},
		{Level: 1, Title: "tabbed", Offset: int64(strings.Index(src, "#\ttabbed"))},
	}
	if len(d.Headings) != len(want) {
		t.Fatalf("%+v", d.Headings)
	}
	for i := range want {
		if d.Headings[i] != want[i] {
			t.Fatalf("heading %d: got %+v want %+v", i, d.Headings[i], want[i])
		}
	}
}
