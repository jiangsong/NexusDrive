package textract

// Office Open XML extraction (docx, xlsx, pptx). Every part is read with the
// streaming encoding/xml decoder: the state machines below keep only the
// current paragraph, row or table cell in memory, never a tree, so a
// malformed or deeply nested part costs bounded CPU and memory rather than
// a stack overflow. encoding/xml does not resolve external entities or
// DTDs, so XXE-style parts are inert. Element names are matched on their
// local part only; the namespace URIs vary between strict and transitional
// OOXML and carry no information the extractor needs.

import (
	"context"
	"encoding/csv"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// maxSharedStrings bounds the xlsx shared-string table; a larger one is
// treated as hostile. A variable so tests can lower it.
var maxSharedStrings = 1_000_000

// ctxCheckEvery is the token interval at which the decoders poll ctx.
const ctxCheckEvery = 1000

var headingStyle = regexp.MustCompile(`(?i)^heading([1-6])$`)

// docBuilder accumulates Doc.Text line by line, records headings with their
// offsets and stops accepting input once MaxTextBytes is reached, so the
// extractors can stop parsing early.
type docBuilder struct {
	sb        strings.Builder
	headings  []Heading
	limit     int64
	truncated bool
}

func newDocBuilder(opt Options) *docBuilder {
	return &docBuilder{limit: opt.MaxTextBytes}
}

// full reports whether the text limit has been reached.
func (b *docBuilder) full() bool {
	return b.limit > 0 && int64(b.sb.Len()) >= b.limit
}

// line appends s and a newline. Past the limit the builder marks the doc
// truncated and drops the input.
func (b *docBuilder) line(s string) {
	if b.full() {
		b.truncated = true
		return
	}
	b.sb.WriteString(s)
	b.sb.WriteByte('\n')
}

// heading appends a markdown heading line and records it in the outline.
func (b *docBuilder) heading(level int, title string) {
	if b.full() {
		b.truncated = true
		return
	}
	b.headings = append(b.headings, Heading{Level: level, Title: title, Offset: int64(b.sb.Len())})
	b.line(strings.Repeat("#", level) + " " + title)
}

// doc finalises the text: it is cut on a rune boundary at the limit, and an
// empty result is ErrNoText.
func (b *docBuilder) doc() (Doc, error) {
	text := b.sb.String()
	if b.limit > 0 && int64(len(text)) > b.limit {
		cut := text[:b.limit]
		text = cut[:lastRuneBoundary([]byte(cut))]
		b.truncated = true
	}
	if strings.TrimSpace(text) == "" {
		return Doc{}, ErrNoText
	}
	d := Doc{Text: text, Truncated: b.truncated, OffsetKind: "text"}
	for _, h := range b.headings {
		if h.Offset < int64(len(text)) {
			d.Headings = append(d.Headings, h)
		}
	}
	return d, nil
}

// xmlPart opens an archive entry and wraps it in a streaming decoder.
type xmlPart struct {
	name string
	rc   io.ReadCloser
	dec  *xml.Decoder
	ctx  context.Context
	n    int
}

func openPart(ctx context.Context, g *guardedZip, name string) (*xmlPart, error) {
	rc, err := g.Open(name)
	if err != nil {
		return nil, err
	}
	dec := xml.NewDecoder(rc)
	dec.Strict = true
	return &xmlPart{name: name, rc: rc, dec: dec, ctx: ctx}, nil
}

// next returns the next token, or (nil, nil) at the end of the part. Every
// ctxCheckEvery tokens it polls the context. Errors are wrapped with the part
// name; the sentinel and context errors stay recognisable with errors.Is.
func (p *xmlPart) next() (xml.Token, error) {
	p.n++
	if p.n%ctxCheckEvery == 0 {
		if err := p.ctx.Err(); err != nil {
			return nil, err
		}
	}
	tok, err := p.dec.Token()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, nil
		}
		if errors.Is(err, ErrZipBomb) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, fmt.Errorf("textract: %s: %w", p.name, err)
	}
	return tok, nil
}

func (p *xmlPart) close() { p.rc.Close() }

func attr(se xml.StartElement, local string) string {
	for _, a := range se.Attr {
		if a.Name.Local == local {
			return a.Value
		}
	}
	return ""
}

// extractOffice is the dispatch target for the three container kinds.
func extractOffice(ctx context.Context, kind Kind, r io.ReaderAt, size int64, opt Options) (Doc, error) {
	g, err := openZip(r, size, opt)
	if err != nil {
		return Doc{}, err
	}
	switch kind {
	case KindDocx:
		return extractDocx(ctx, g, opt)
	case KindXlsx:
		return extractXlsx(ctx, g, opt)
	default:
		return extractPptx(ctx, g, opt)
	}
}

// missingPart maps an absent mandatory part to ErrUnsupported: the archive
// is not the document its extension promised.
func missingPart(err error, kind Kind, name string) error {
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: %s without %s", ErrUnsupported, kind, name)
	}
	return err
}

// docxRow is one table row under construction; nested tables push their own.
type docxRow struct {
	cells []string
	cell  []string // paragraphs of the cell being built
}

// extractDocx turns word/document.xml into one line per paragraph. Paragraphs
// styled Heading1..6 become markdown headings; table rows become
// "| a | b |" lines with the cell's paragraphs joined by spaces.
func extractDocx(ctx context.Context, g *guardedZip, opt Options) (Doc, error) {
	if err := ctx.Err(); err != nil {
		return Doc{}, err
	}
	const part = "word/document.xml"
	p, err := openPart(ctx, g, part)
	if err != nil {
		return Doc{}, missingPart(err, KindDocx, part)
	}
	defer p.close()

	b := newDocBuilder(opt)
	var (
		para   strings.Builder
		inPara bool
		level  int
		inText bool
		rows   []*docxRow
	)
	for !b.full() {
		tok, err := p.next()
		if err != nil {
			return Doc{}, err
		}
		if tok == nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "p":
				para.Reset()
				inPara, level = true, 0
			case "pStyle":
				if m := headingStyle.FindStringSubmatch(attr(t, "val")); m != nil && inPara {
					level = int(m[1][0] - '0')
				}
			case "t":
				inText = true
			case "tab", "br", "cr":
				if inPara {
					para.WriteByte(' ')
				}
			case "tr":
				rows = append(rows, &docxRow{})
			case "tc":
				if n := len(rows); n > 0 {
					rows[n-1].cell = rows[n-1].cell[:0]
				}
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				inText = false
			case "p":
				if !inPara {
					break
				}
				inPara = false
				text := strings.TrimSpace(para.String())
				if n := len(rows); n > 0 {
					if text != "" {
						rows[n-1].cell = append(rows[n-1].cell, text)
					}
				} else if text == "" {
					// Empty paragraphs carry no text.
				} else if level > 0 {
					b.heading(level, text)
				} else {
					b.line(text)
				}
			case "tc":
				if n := len(rows); n > 0 {
					rows[n-1].cells = append(rows[n-1].cells, strings.Join(rows[n-1].cell, " "))
					rows[n-1].cell = rows[n-1].cell[:0]
				}
			case "tr":
				if n := len(rows); n > 0 {
					row := rows[n-1]
					rows = rows[:n-1]
					if strings.TrimSpace(strings.Join(row.cells, "")) != "" {
						b.line("| " + strings.Join(row.cells, " | ") + " |")
					}
				}
			}
		case xml.CharData:
			if inText && inPara && (b.limit <= 0 || int64(para.Len()) < b.limit) {
				para.Write(t)
			}
		}
	}
	return b.doc()
}

// extractXlsx emits "## <sheet>" followed by one CSV line per row for every
// worksheet, in workbook order, at most opt.MaxSheetRows rows per sheet.
func extractXlsx(ctx context.Context, g *guardedZip, opt Options) (Doc, error) {
	if err := ctx.Err(); err != nil {
		return Doc{}, err
	}
	sheets, err := xlsxSheets(ctx, g)
	if err != nil {
		return Doc{}, err
	}
	if len(sheets) == 0 {
		return Doc{}, fmt.Errorf("%w: xlsx without worksheets", ErrUnsupported)
	}
	shared, err := xlsxSharedStrings(ctx, g)
	if err != nil {
		return Doc{}, err
	}
	b := newDocBuilder(opt)
	for _, s := range sheets {
		if b.full() {
			break
		}
		b.heading(2, s.name)
		if err := xlsxSheet(ctx, g, s.part, shared, opt, b); err != nil {
			return Doc{}, err
		}
	}
	return b.doc()
}

type xlsxSheetRef struct {
	name string
	part string
}

var sheetPartNum = regexp.MustCompile(`^xl/worksheets/sheet(\d+)\.xml$`)

// xlsxSheets lists worksheets in workbook order. Names come from
// xl/workbook.xml and parts from the workbook relationships; when either is
// missing the sheetN.xml parts are used in numeric order, paired with the
// workbook's names by position.
func xlsxSheets(ctx context.Context, g *guardedZip) ([]xlsxSheetRef, error) {
	parts := numberedParts(g, sheetPartNum)
	rels, err := xlsxRels(ctx, g)
	if err != nil {
		return nil, err
	}
	p, err := openPart(ctx, g, "xl/workbook.xml")
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		out := make([]xlsxSheetRef, 0, len(parts))
		for _, np := range parts {
			out = append(out, xlsxSheetRef{name: "Sheet" + strconv.Itoa(np.n), part: np.name})
		}
		return out, nil
	}
	defer p.close()
	var out []xlsxSheetRef
	used := map[string]bool{}
	for {
		tok, err := p.next()
		if err != nil {
			return nil, err
		}
		if tok == nil {
			break
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "sheet" {
			continue
		}
		ref := xlsxSheetRef{name: attr(se, "name")}
		if target, ok := rels[attr(se, "id")]; ok {
			ref.part = target
		} else if i := len(out); i < len(parts) {
			ref.part = parts[i].name
		}
		if ref.part == "" || used[ref.part] {
			continue
		}
		used[ref.part] = true
		if ref.name == "" {
			ref.name = "Sheet" + strconv.Itoa(len(out)+1)
		}
		out = append(out, ref)
	}
	return out, nil
}

// xlsxRels maps relationship ids to archive entry names; a missing rels part
// yields an empty map.
func xlsxRels(ctx context.Context, g *guardedZip) (map[string]string, error) {
	p, err := openPart(ctx, g, "xl/_rels/workbook.xml.rels")
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	defer p.close()
	rels := map[string]string{}
	for {
		tok, err := p.next()
		if err != nil {
			return nil, err
		}
		if tok == nil {
			return rels, nil
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "Relationship" {
			continue
		}
		target := attr(se, "Target")
		if strings.HasPrefix(target, "/") {
			target = strings.TrimPrefix(target, "/")
		} else {
			target = "xl/" + target
		}
		if id := attr(se, "Id"); id != "" && target != "" {
			rels[id] = target
		}
	}
}

// xlsxSharedStrings loads xl/sharedStrings.xml; each <si> is the
// concatenation of its <t> runs, phonetic guides (<rPh>) excluded.
func xlsxSharedStrings(ctx context.Context, g *guardedZip) ([]string, error) {
	p, err := openPart(ctx, g, "xl/sharedStrings.xml")
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer p.close()
	var (
		out    []string
		cur    strings.Builder
		inSI   bool
		inT    bool
		phDeep int
	)
	for {
		tok, err := p.next()
		if err != nil {
			return nil, err
		}
		if tok == nil {
			return out, nil
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "si":
				cur.Reset()
				inSI = true
			case "rPh":
				phDeep++
			case "t":
				inT = true
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "si":
				if inSI {
					if len(out) >= maxSharedStrings {
						return nil, fmt.Errorf("%w: more than %d shared strings", ErrZipBomb, maxSharedStrings)
					}
					out = append(out, cur.String())
				}
				inSI = false
			case "rPh":
				if phDeep > 0 {
					phDeep--
				}
			case "t":
				inT = false
			}
		case xml.CharData:
			if inSI && inT && phDeep == 0 {
				cur.Write(t)
			}
		}
	}
}

// xlsxSheet streams one worksheet's rows into b as CSV lines.
func xlsxSheet(ctx context.Context, g *guardedZip, part string, shared []string, opt Options, b *docBuilder) error {
	p, err := openPart(ctx, g, part)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	defer p.close()
	var (
		cells    []string
		val      strings.Builder
		typ      string
		inCell   bool
		inV      bool
		inIS     bool
		inT      bool
		rows     int
		line     strings.Builder
		csvw     = csv.NewWriter(&line)
		nonBlank bool
	)
	for {
		if b.full() {
			return nil
		}
		tok, err := p.next()
		if err != nil {
			return err
		}
		if tok == nil {
			return nil
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "row":
				cells, nonBlank = cells[:0], false
			case "c":
				val.Reset()
				typ, inCell = attr(t, "t"), true
			case "v":
				inV = inCell
			case "is":
				inIS = inCell
			case "t":
				inT = inIS
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "c":
				if !inCell {
					break
				}
				inCell = false
				v := val.String()
				if typ == "s" {
					idx, err := strconv.Atoi(strings.TrimSpace(v))
					if err == nil && idx >= 0 && idx < len(shared) {
						v = shared[idx]
					} else {
						v = ""
					}
				}
				if v != "" {
					nonBlank = true
				}
				cells = append(cells, v)
			case "v":
				inV = false
			case "is":
				inIS, inT = false, false
			case "t":
				inT = false
			case "row":
				if !nonBlank {
					break
				}
				if opt.MaxSheetRows > 0 && rows >= opt.MaxSheetRows {
					b.truncated = true
					return nil
				}
				rows++
				line.Reset()
				csvw.Write(cells)
				csvw.Flush()
				b.line(strings.TrimSuffix(line.String(), "\n"))
			}
		case xml.CharData:
			if inCell && (inV || inT) && (b.limit <= 0 || int64(val.Len()) < b.limit) {
				val.Write(t)
			}
		}
	}
}

var slidePartNum = regexp.MustCompile(`^ppt/slides/slide(\d+)\.xml$`)

// extractPptx emits "## Slide N" per slide in numeric order followed by one
// line per <a:p> paragraph, runs concatenated.
func extractPptx(ctx context.Context, g *guardedZip, opt Options) (Doc, error) {
	if err := ctx.Err(); err != nil {
		return Doc{}, err
	}
	slides := numberedParts(g, slidePartNum)
	if len(slides) == 0 {
		return Doc{}, fmt.Errorf("%w: pptx without slides", ErrUnsupported)
	}
	b := newDocBuilder(opt)
	for _, s := range slides {
		if b.full() {
			break
		}
		b.heading(2, "Slide "+strconv.Itoa(s.n))
		if err := pptxSlide(ctx, g, s.name, b); err != nil {
			return Doc{}, err
		}
	}
	return b.doc()
}

// pptxSlide streams one slide's <a:t> text into b, one line per paragraph.
func pptxSlide(ctx context.Context, g *guardedZip, part string, b *docBuilder) error {
	p, err := openPart(ctx, g, part)
	if err != nil {
		return err
	}
	defer p.close()
	var (
		para strings.Builder
		inT  bool
	)
	flush := func() {
		if text := strings.TrimSpace(para.String()); text != "" {
			b.line(text)
		}
		para.Reset()
	}
	for {
		if b.full() {
			return nil
		}
		tok, err := p.next()
		if err != nil {
			return err
		}
		if tok == nil {
			flush()
			return nil
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "t":
				inT = true
			case "br", "tab":
				para.WriteByte(' ')
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				inT = false
			case "p":
				flush()
			}
		case xml.CharData:
			if inT && (b.limit <= 0 || int64(para.Len()) < b.limit) {
				para.Write(t)
			}
		}
	}
}

type numberedPart struct {
	name string
	n    int
}

// numberedParts lists the entries matching re (whose first group is a
// number) sorted by that number.
func numberedParts(g *guardedZip, re *regexp.Regexp) []numberedPart {
	var out []numberedPart
	for _, name := range g.Names("") {
		m := re.FindStringSubmatch(name)
		if m == nil {
			continue
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		out = append(out, numberedPart{name: name, n: n})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].n < out[j].n })
	return out
}
