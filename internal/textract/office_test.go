package textract

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// buildZip writes files into an in-memory archive in name order so that
// fixtures are deterministic.
func buildZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		w, err := zw.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, files[n]); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func extractBytes(t *testing.T, kind Kind, b []byte) (Doc, error) {
	t.Helper()
	return extractBytesWith(t, kind, b, DefaultOptions())
}

func extractBytesWith(t *testing.T, kind Kind, b []byte, opt Options) (Doc, error) {
	t.Helper()
	return Extract(context.Background(), kind, bytes.NewReader(b), int64(len(b)), opt)
}

const docxBody = `<?xml version="1.0"?><w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>
<w:p><w:pPr><w:pStyle w:val="Heading1"/></w:pPr><w:r><w:t>第二章 设计</w:t></w:r></w:p>
<w:p><w:r><w:t>正文第一段</w:t></w:r><w:r><w:t xml:space="preserve"> 继续</w:t></w:r></w:p>
<w:tbl><w:tr><w:tc><w:p><w:r><w:t>a</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>b</w:t></w:r></w:p></w:tc></w:tr></w:tbl>
</w:body></w:document>`

func docxZip(t *testing.T, body string) []byte {
	t.Helper()
	return buildZip(t, map[string]string{"word/document.xml": body})
}

func TestDocxHeading1BecomesAMarkdownHeading(t *testing.T) {
	d, err := extractBytes(t, KindDocx, docxZip(t, docxBody))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d.Text, "# 第二章 设计\n") || !strings.Contains(d.Text, "正文第一段 继续") {
		t.Fatalf("%q", d.Text)
	}
	if len(d.Headings) != 1 || d.Headings[0].Title != "第二章 设计" || d.Headings[0].Level != 1 {
		t.Fatalf("%+v", d.Headings)
	}
	if off := d.Headings[0].Offset; !strings.HasPrefix(d.Text[off:], "# 第二章 设计\n") {
		t.Fatalf("heading offset %d points at %q", off, d.Text[off:])
	}
	if !strings.Contains(d.Text, "| a | b |") {
		t.Fatalf("table row missing: %q", d.Text)
	}
	if d.OffsetKind != "text" {
		t.Fatal(d.OffsetKind)
	}
	if d.Truncated {
		t.Fatal("unexpected truncation")
	}
}

func TestDocxHeadingLevelsTablesAndRunsOutsideParagraphs(t *testing.T) {
	var sb strings.Builder
	sb.WriteString(`<w:document xmlns:w="w"><w:body>`)
	for lvl := 1; lvl <= 6; lvl++ {
		fmt.Fprintf(&sb, `<w:p><w:pPr><w:pStyle w:val="Heading%d"/></w:pPr><w:r><w:t>H%d</w:t></w:r></w:p>`, lvl, lvl)
	}
	// Heading7 and a lowercase style id: only 1..6 are outline levels.
	sb.WriteString(`<w:p><w:pPr><w:pStyle w:val="Heading7"/></w:pPr><w:r><w:t>H7</w:t></w:r></w:p>`)
	sb.WriteString(`<w:p><w:pPr><w:pStyle w:val="heading2"/></w:pPr><w:r><w:t>lower</w:t></w:r></w:p>`)
	// A tab and a break inside a run stay on the paragraph's line.
	sb.WriteString(`<w:p><w:r><w:t>x</w:t><w:tab/><w:t>y</w:t><w:br/><w:t>z</w:t></w:r></w:p>`)
	// Tracked deletions and field codes are not visible text.
	sb.WriteString(`<w:p><w:r><w:delText>gone</w:delText><w:instrText>PAGE</w:instrText><w:t>kept</w:t></w:r></w:p>`)
	// Two rows; the second cell of the first row holds two paragraphs.
	sb.WriteString(`<w:tbl><w:tr><w:tc><w:p><w:r><w:t>r1c1</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>p1</w:t></w:r></w:p><w:p><w:r><w:t>p2</w:t></w:r></w:p></w:tc></w:tr>` +
		`<w:tr><w:tc><w:p><w:r><w:t>r2c1</w:t></w:r></w:p></w:tc><w:tc><w:p/></w:tc></w:tr></w:tbl>`)
	sb.WriteString(`<w:p><w:r><w:t>after</w:t></w:r></w:p></w:body></w:document>`)

	d, err := extractBytes(t, KindDocx, docxZip(t, sb.String()))
	if err != nil {
		t.Fatal(err)
	}
	want := "# H1\n## H2\n### H3\n#### H4\n##### H5\n###### H6\nH7\n## lower\nx y z\nkept\n| r1c1 | p1 p2 |\n| r2c1 |  |\nafter\n"
	if d.Text != want {
		t.Fatalf("got\n%q\nwant\n%q", d.Text, want)
	}
	if len(d.Headings) != 7 {
		t.Fatalf("%+v", d.Headings)
	}
	for i, h := range d.Headings {
		if !strings.HasPrefix(d.Text[h.Offset:], strings.Repeat("#", h.Level)+" "+h.Title+"\n") {
			t.Fatalf("heading %d %+v does not point at its line", i, h)
		}
	}
	if d.Headings[6].Level != 2 || d.Headings[6].Title != "lower" {
		t.Fatalf("%+v", d.Headings[6])
	}
}

func TestDocxWithoutTextIsNoText(t *testing.T) {
	z := docxZip(t, `<w:document xmlns:w="w"><w:body/></w:document>`)
	if _, err := extractBytes(t, KindDocx, z); !errors.Is(err, ErrNoText) {
		t.Fatalf("got %v", err)
	}
	// Whitespace-only runs are no text either.
	z = docxZip(t, `<w:document xmlns:w="w"><w:body><w:p><w:r><w:t xml:space="preserve">   </w:t></w:r></w:p></w:body></w:document>`)
	if _, err := extractBytes(t, KindDocx, z); !errors.Is(err, ErrNoText) {
		t.Fatalf("got %v", err)
	}
}

func TestOfficeContainerWithoutItsMainPartIsUnsupported(t *testing.T) {
	z := buildZip(t, map[string]string{"[Content_Types].xml": `<Types/>`})
	for _, kind := range []Kind{KindDocx, KindXlsx, KindPptx} {
		if _, err := extractBytes(t, kind, z); !errors.Is(err, ErrUnsupported) {
			t.Errorf("%s: got %v", kind, err)
		}
	}
}

func TestXlsxSheetsAndSharedStrings(t *testing.T) {
	z := buildZip(t, map[string]string{
		"xl/workbook.xml":          `<workbook><sheets><sheet name="预算" sheetId="1" r:id="rId1" xmlns:r="r"/></sheets></workbook>`,
		"xl/sharedStrings.xml":     `<sst><si><t>项目</t></si><si><t>金额</t></si></sst>`,
		"xl/worksheets/sheet1.xml": `<worksheet><sheetData><row><c t="s"><v>0</v></c><c t="s"><v>1</v></c></row><row><c t="inlineStr"><is><t>服务器</t></is></c><c><v>1200</v></c></row></sheetData></worksheet>`,
	})
	d, err := extractBytes(t, KindXlsx, z)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d.Text, "## 预算\n") || !strings.Contains(d.Text, "项目,金额\n") || !strings.Contains(d.Text, "服务器,1200\n") {
		t.Fatalf("%q", d.Text)
	}
	if len(d.Headings) != 1 || d.Headings[0].Level != 2 || d.Headings[0].Title != "预算" || !strings.HasPrefix(d.Text[d.Headings[0].Offset:], "## 预算\n") {
		t.Fatalf("%+v", d.Headings)
	}
	if d.OffsetKind != "text" || d.Truncated {
		t.Fatalf("%+v", d)
	}
}

func TestXlsxSharedStringRunsAndOddCells(t *testing.T) {
	z := buildZip(t, map[string]string{
		"xl/workbook.xml": `<workbook><sheets><sheet name="S" sheetId="1"/></sheets></workbook>`,
		// Rich-text runs are concatenated; phonetic guides (rPh) are not text.
		"xl/sharedStrings.xml": `<sst><si><r><t>Hel</t></r><r><t>lo</t></r><rPh><t>ignored</t></rPh></si></sst>`,
		"xl/worksheets/sheet1.xml": `<worksheet><sheetData>` +
			`<row><c t="s"><v>0</v></c><c t="s"><v>99</v></c><c t="s"><v>x</v></c><c><f>SUM(A1)</f><v>3</v></c></row>` +
			`<row/>` + // no cells at all: no line
			`<row><c/><c/></row>` + // only empty cells: no line
			`<row><c t="inlineStr"><is><t>a,b</t></is></c><c t="str"><v>say "hi"</v></c></row>` +
			`</sheetData></worksheet>`,
	})
	d, err := extractBytes(t, KindXlsx, z)
	if err != nil {
		t.Fatal(err)
	}
	want := "## S\nHello,,,3\n\"a,b\",\"say \"\"hi\"\"\"\n"
	if d.Text != want {
		t.Fatalf("got %q want %q", d.Text, want)
	}
}

func TestXlsxSheetOrderFollowsWorkbookAndRels(t *testing.T) {
	z := buildZip(t, map[string]string{
		"xl/workbook.xml": `<workbook xmlns:r="r"><sheets><sheet name="Second" sheetId="2" r:id="rId7"/><sheet name="First" sheetId="1" r:id="rId3"/></sheets></workbook>`,
		"xl/_rels/workbook.xml.rels": `<Relationships><Relationship Id="rId3" Type="ws" Target="worksheets/sheet1.xml"/>` +
			`<Relationship Id="rId7" Type="ws" Target="/xl/worksheets/sheet2.xml"/></Relationships>`,
		"xl/worksheets/sheet1.xml": `<worksheet><sheetData><row><c t="inlineStr"><is><t>one</t></is></c></row></sheetData></worksheet>`,
		"xl/worksheets/sheet2.xml": `<worksheet><sheetData><row><c t="inlineStr"><is><t>two</t></is></c></row></sheetData></worksheet>`,
	})
	d, err := extractBytes(t, KindXlsx, z)
	if err != nil {
		t.Fatal(err)
	}
	if d.Text != "## Second\ntwo\n## First\none\n" {
		t.Fatalf("%q", d.Text)
	}
}

func TestXlsxWithoutWorkbookFallsBackToSheetFileOrder(t *testing.T) {
	z := buildZip(t, map[string]string{
		"xl/worksheets/sheet10.xml": `<worksheet><sheetData><row><c><v>10</v></c></row></sheetData></worksheet>`,
		"xl/worksheets/sheet2.xml":  `<worksheet><sheetData><row><c><v>2</v></c></row></sheetData></worksheet>`,
	})
	d, err := extractBytes(t, KindXlsx, z)
	if err != nil {
		t.Fatal(err)
	}
	if d.Text != "## Sheet2\n2\n## Sheet10\n10\n" {
		t.Fatalf("%q", d.Text)
	}
}

func TestXlsxRowsAreCappedPerSheet(t *testing.T) {
	var rows strings.Builder
	for i := 0; i < 10; i++ {
		fmt.Fprintf(&rows, `<row><c><v>%d</v></c></row>`, i)
	}
	sheet := `<worksheet><sheetData>` + rows.String() + `</sheetData></worksheet>`
	z := buildZip(t, map[string]string{
		"xl/workbook.xml":          `<workbook><sheets><sheet name="A" sheetId="1"/><sheet name="B" sheetId="2"/></sheets></workbook>`,
		"xl/worksheets/sheet1.xml": sheet,
		"xl/worksheets/sheet2.xml": sheet,
	})
	opt := DefaultOptions()
	opt.MaxSheetRows = 3
	d, err := extractBytesWith(t, KindXlsx, z, opt)
	if err != nil {
		t.Fatal(err)
	}
	if d.Text != "## A\n0\n1\n2\n## B\n0\n1\n2\n" || !d.Truncated {
		t.Fatalf("%q truncated=%v", d.Text, d.Truncated)
	}
}

func TestXlsxSharedStringTableIsCapped(t *testing.T) {
	if maxSharedStrings != 1_000_000 {
		t.Fatalf("default cap %d", maxSharedStrings)
	}
	defer func(n int) { maxSharedStrings = n }(maxSharedStrings)
	maxSharedStrings = 50
	var sst strings.Builder
	sst.WriteString(`<sst>`)
	for i := 0; i < maxSharedStrings+1; i++ {
		sst.WriteString(`<si><t>x</t></si>`)
	}
	sst.WriteString(`</sst>`)
	z := buildZip(t, map[string]string{
		"xl/workbook.xml":          `<workbook><sheets><sheet name="A" sheetId="1"/></sheets></workbook>`,
		"xl/sharedStrings.xml":     sst.String(),
		"xl/worksheets/sheet1.xml": `<worksheet><sheetData><row><c t="s"><v>0</v></c></row></sheetData></worksheet>`,
	})
	if _, err := extractBytes(t, KindXlsx, z); !errors.Is(err, ErrZipBomb) {
		t.Fatalf("got %v", err)
	}
}

func TestPptxSlidesInNumericOrder(t *testing.T) {
	z := buildZip(t, map[string]string{
		"ppt/slides/slide10.xml": `<p:sld xmlns:p="p" xmlns:a="a"><a:t>ten</a:t></p:sld>`,
		"ppt/slides/slide2.xml":  `<p:sld xmlns:p="p" xmlns:a="a"><a:t>two</a:t></p:sld>`,
	})
	d, err := extractBytes(t, KindPptx, z)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Index(d.Text, "## Slide 2") > strings.Index(d.Text, "## Slide 10") {
		t.Fatalf("%q", d.Text)
	}
	if d.Text != "## Slide 2\ntwo\n## Slide 10\nten\n" {
		t.Fatalf("%q", d.Text)
	}
	if len(d.Headings) != 2 || d.Headings[0].Title != "Slide 2" || d.Headings[1].Title != "Slide 10" || d.Headings[1].Offset != int64(len("## Slide 2\ntwo\n")) {
		t.Fatalf("%+v", d.Headings)
	}
}

func TestPptxParagraphsBecomeLinesAndRunsJoin(t *testing.T) {
	z := buildZip(t, map[string]string{
		"ppt/slides/slide1.xml": `<p:sld xmlns:p="p" xmlns:a="a"><p:txBody>` +
			`<a:p><a:r><a:t>Ti</a:t></a:r><a:r><a:t>tle</a:t></a:r></a:p>` +
			`<a:p><a:r><a:t>first</a:t></a:r><a:br/><a:r><a:t>second</a:t></a:r></a:p>` +
			`<a:p/>` +
			`</p:txBody></p:sld>`,
		// Other slide-related parts (layouts, notes, rels) are not slides.
		"ppt/slideLayouts/slideLayout1.xml": `<p:sldLayout xmlns:p="p" xmlns:a="a"><a:t>layout</a:t></p:sldLayout>`,
		"ppt/notesSlides/notesSlide1.xml":   `<p:notes xmlns:p="p" xmlns:a="a"><a:t>notes</a:t></p:notes>`,
		"ppt/slides/_rels/slide1.xml.rels":  `<Relationships/>`,
	})
	d, err := extractBytes(t, KindPptx, z)
	if err != nil {
		t.Fatal(err)
	}
	if d.Text != "## Slide 1\nTitle\nfirst second\n" {
		t.Fatalf("%q", d.Text)
	}
}

func TestZipWithTooManyEntriesFails(t *testing.T) {
	files := map[string]string{"word/document.xml": docxBody}
	for i := 0; i < 4096; i++ {
		files[fmt.Sprintf("junk/%05d", i)] = ""
	}
	if _, err := extractBytes(t, KindDocx, buildZip(t, files)); !errors.Is(err, ErrZipBomb) {
		t.Fatalf("got %v", err)
	}
}

func TestZipEntryOverTheLimitFails(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(w, `<w:document xmlns:w="w"><w:body><w:p><w:r><w:t>`)
	w.Write(bytes.Repeat([]byte("A"), 70<<20)) // compresses to almost nothing
	io.WriteString(w, `</w:t></w:r></w:p></w:body></w:document>`)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := extractBytes(t, KindDocx, buf.Bytes()); !errors.Is(err, ErrZipBomb) {
		t.Fatalf("got %v", err)
	}
}

// rawZip writes one entry whose header claims declared uncompressed bytes
// while the deflate stream actually inflates to body.
func rawZip(t *testing.T, name string, body []byte, declared uint64) []byte {
	t.Helper()
	var comp bytes.Buffer
	fw, err := flate.NewWriter(&comp, flate.BestCompression)
	if err != nil {
		t.Fatal(err)
	}
	fw.Write(body)
	fw.Close()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.CreateRaw(&zip.FileHeader{
		Name:               name,
		Method:             zip.Deflate,
		CompressedSize64:   uint64(comp.Len()),
		UncompressedSize64: declared,
	})
	if err != nil {
		t.Fatal(err)
	}
	w.Write(comp.Bytes())
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestZipEntryLyingAboutItsSizeFails(t *testing.T) {
	body := append([]byte(`<w:document xmlns:w="w"><w:body><w:p><w:r><w:t>`), bytes.Repeat([]byte("A"), 4<<20)...)
	body = append(body, `</w:t></w:r></w:p></w:body></w:document>`...)
	z := rawZip(t, "word/document.xml", body, 16)
	opt := DefaultOptions()
	opt.MaxZipEntryBytes = 1 << 20 // the declared 16 bytes pass the header check
	if _, err := extractBytesWith(t, KindDocx, z, opt); !errors.Is(err, ErrZipBomb) {
		t.Fatalf("got %v", err)
	}
}

func TestZipTotalOverTheLimitFails(t *testing.T) {
	files := map[string]string{}
	slide := `<p:sld xmlns:p="p" xmlns:a="a"><a:t>` + strings.Repeat("A", 1<<20) + `</a:t></p:sld>`
	for i := 1; i <= 4; i++ {
		files[fmt.Sprintf("ppt/slides/slide%d.xml", i)] = slide
	}
	z := buildZip(t, files)
	opt := DefaultOptions()
	opt.MaxTextBytes = 16 << 20
	opt.MaxZipTotalBytes = 3 << 20 // each slide alone is fine, the four are not
	if _, err := extractBytesWith(t, KindPptx, z, opt); !errors.Is(err, ErrZipBomb) {
		t.Fatalf("got %v", err)
	}
	opt.MaxZipTotalBytes = 8 << 20
	if _, err := extractBytesWith(t, KindPptx, z, opt); err != nil {
		t.Fatalf("got %v", err)
	}
}

// The streaming counter must fire on bytes actually delivered, independently
// of whatever the archive header claimed.
func TestGuardReaderCountsDeliveredBytes(t *testing.T) {
	g := &guardedZip{opt: Options{MaxZipEntryBytes: 10, MaxZipTotalBytes: 100}}
	r := &guardReader{rc: io.NopCloser(strings.NewReader(strings.Repeat("x", 11))), g: g, name: "e", declared: 1 << 30}
	if _, err := io.ReadAll(r); !errors.Is(err, ErrZipBomb) {
		t.Fatalf("entry limit: got %v", err)
	}
	g = &guardedZip{opt: Options{MaxZipEntryBytes: 100, MaxZipTotalBytes: 15}}
	for i := 0; i < 2; i++ {
		r := &guardReader{rc: io.NopCloser(strings.NewReader(strings.Repeat("x", 8))), g: g, name: "e", declared: 1 << 30}
		_, err := io.ReadAll(r)
		if i == 0 && err != nil {
			t.Fatalf("first entry: %v", err)
		}
		if i == 1 && !errors.Is(err, ErrZipBomb) {
			t.Fatalf("total limit: got %v", err)
		}
	}
}

func TestGuardedZipOpenReportsMissingEntries(t *testing.T) {
	z := buildZip(t, map[string]string{"a.txt": "a"})
	g, err := openZip(bytes.NewReader(z), int64(len(z)), DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Open("missing"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("got %v", err)
	}
	if names := g.Names("a"); len(names) != 1 || names[0] != "a.txt" {
		t.Fatalf("%v", names)
	}
}

func TestMalformedOfficeXMLFailsWithoutPanicking(t *testing.T) {
	deep := strings.Repeat("<w:p>", 200000)
	for _, body := range []string{
		``,
		`garbage`,
		`<w:document xmlns:w="w"><w:body><w:p><w:r><w:t>open`,
		`<w:document xmlns:w="w"><w:body>` + deep + `<w:r><w:t>x</w:t></w:r>`,
		`<w:document xmlns:w="w"><w:body><w:tbl><w:tr><w:tc></w:tr></w:tc></w:tbl></w:body></w:document>`,
		`<?xml version="1.0" encoding="latin1"?><w:document xmlns:w="w"><w:body><w:p><w:r><w:t>x</w:t></w:r></w:p></w:body></w:document>`,
		"\x00\x01\x02",
	} {
		_, err := extractBytes(t, KindDocx, docxZip(t, body))
		if err == nil {
			t.Errorf("%.40q: expected an error", body)
		}
	}
	// Unclosed elements in a sheet or a slide are equally harmless.
	z := buildZip(t, map[string]string{
		"xl/workbook.xml":          `<workbook><sheets><sheet name="A"`,
		"xl/sharedStrings.xml":     `<sst><si><t>`,
		"xl/worksheets/sheet1.xml": `<worksheet><sheetData><row><c><v>1`,
	})
	if _, err := extractBytes(t, KindXlsx, z); err == nil {
		t.Error("xlsx: expected an error")
	}
	z = buildZip(t, map[string]string{"ppt/slides/slide1.xml": `<p:sld><a:t>x`})
	if _, err := extractBytes(t, KindPptx, z); err == nil {
		t.Error("pptx: expected an error")
	}
	// A directory entry and a non-zip file are rejected, not dereferenced.
	if _, err := Extract(context.Background(), KindDocx, strings.NewReader("PK\x03\x04junk"), 8, DefaultOptions()); err == nil {
		t.Error("truncated zip: expected an error")
	}
}

func TestOfficeTextIsTruncatedOnARuneBoundary(t *testing.T) {
	var sb strings.Builder
	sb.WriteString(`<w:document xmlns:w="w"><w:body>`)
	for i := 0; i < 100; i++ {
		sb.WriteString(`<w:p><w:pPr><w:pStyle w:val="Heading2"/></w:pPr><w:r><w:t>标题</w:t></w:r></w:p><w:p><w:r><w:t>中文正文</w:t></w:r></w:p>`)
	}
	sb.WriteString(`</w:body></w:document>`)
	opt := DefaultOptions()
	opt.MaxTextBytes = 100
	d, err := extractBytesWith(t, KindDocx, docxZip(t, sb.String()), opt)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Truncated || int64(len(d.Text)) > opt.MaxTextBytes || !utf8.ValidString(d.Text) || len(d.Text) < 90 {
		t.Fatalf("%q truncated=%v", d.Text, d.Truncated)
	}
	if len(d.Headings) == 0 {
		t.Fatal("headings before the cut must survive")
	}
	for _, h := range d.Headings {
		if h.Offset >= int64(len(d.Text)) || !strings.HasPrefix(d.Text[h.Offset:], "## ") {
			t.Fatalf("heading %+v beyond the text or misplaced", h)
		}
	}
}

func TestOfficeExtractionHonoursTheTimeout(t *testing.T) {
	opt := DefaultOptions()
	opt.Timeout = time.Nanosecond
	z := docxZip(t, docxBody)
	if _, err := extractBytesWith(t, KindDocx, z, opt); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
}
