package textract

import (
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ChunkerVer is bumped whenever ChunkDoc would cut an unchanged Doc
// differently, so the indexer knows to re-chunk documents built by an older
// build even when the extracted text is unchanged.
const ChunkerVer = 1

// Chunk is one retrieval unit of a Doc: a window of Doc.Text labelled with
// the heading path it falls under. Offsets are byte offsets into Doc.Text
// and always sit on rune boundaries; when Doc.OffsetKind is "file" they are
// file offsets too, so a hit's StartOff can be handed to a ranged read of
// the file and yields the same text.
type Chunk struct {
	Seq      int
	StartOff int64  // byte offset into Doc.Text (== file offset when Doc.OffsetKind == "file")
	EndOff   int64  // exclusive
	Heading  string // nearest enclosing heading path, e.g. "第二章 > 2.1 范围"; "" before the first heading
	Text     string // == Doc.Text[StartOff:EndOff]
}

// ChunkOptions sizes the windows ChunkDoc cuts. Sizes are in runes so that
// CJK and Latin text get comparable chunks. Zero fields take the defaults
// from DefaultChunkOptions, so the zero value chunks like the defaults; a
// negative Overlap disables overlapping.
type ChunkOptions struct {
	Window     int // runes, default 800
	Overlap    int // runes shared by consecutive chunks of one section, default 100, < 0 for none
	CodeWindow int // runes, used instead of Window for KindCode, default 1200
}

// DefaultChunkOptions returns the sizes from TODO.md T-37: 800-rune windows
// overlapping by 100, with wider windows for source code.
func DefaultChunkOptions() ChunkOptions {
	return ChunkOptions{Window: 800, Overlap: 100, CodeWindow: 1200}
}

// normalized fills zero fields and keeps Overlap small enough that every
// window makes progress (the boundary search never ends a chunk before
// half a window, so an overlap under half a window always advances).
func (o ChunkOptions) normalized() ChunkOptions {
	def := DefaultChunkOptions()
	if o.Window <= 0 {
		o.Window = def.Window
	}
	if o.CodeWindow <= 0 {
		o.CodeWindow = def.CodeWindow
	}
	if o.Overlap == 0 {
		o.Overlap = def.Overlap
	}
	if o.Overlap < 0 {
		o.Overlap = 0
	}
	if o.Overlap >= o.Window/2 {
		o.Overlap = o.Window/2 - 1
	}
	if o.Overlap >= o.CodeWindow/2 {
		o.Overlap = o.CodeWindow/2 - 1
	}
	if o.Overlap < 0 {
		o.Overlap = 0
	}
	return o
}

// ChunkDoc cuts d into chunks. The text is first split into sections at the
// headings of d (the text before the first heading is an unlabelled
// section), so no chunk straddles a heading; each section is then cut into
// windows of opt.Window runes (opt.CodeWindow for KindCode) that prefer to
// end at a paragraph break or sentence end, and consecutive windows of one
// section share opt.Overlap runes. The result is a pure function of its
// arguments: the same Doc and options always produce the same chunks.
func ChunkDoc(d Doc, kind Kind, opt ChunkOptions) []Chunk {
	if strings.TrimSpace(d.Text) == "" {
		return nil
	}
	opt = opt.normalized()
	window := opt.Window
	if kind == KindCode {
		window = opt.CodeWindow
	}
	var out []Chunk
	for _, s := range sections(d) {
		for _, w := range windows(d.Text, s.start, s.end, window, opt.Overlap, kind == KindCode) {
			if strings.TrimSpace(d.Text[w.start:w.end]) == "" {
				continue
			}
			out = append(out, Chunk{
				Seq:      len(out),
				StartOff: int64(w.start),
				EndOff:   int64(w.end),
				Heading:  s.heading,
				Text:     d.Text[w.start:w.end],
			})
		}
	}
	return out
}

// span is a half-open byte range of a Doc's text.
type span struct{ start, end int }

// section is a span of Doc.Text under one heading path. The heading line
// itself belongs to the section it opens.
type section struct {
	span
	heading string
}

// sections splits d.Text at its headings and labels every piece with the
// path of headings enclosing it. Headings are taken in offset order;
// entries outside the text or not on a rune boundary (which no extractor
// produces, but the index stores Docs across builds) are ignored.
func sections(d Doc) []section {
	hs := make([]Heading, 0, len(d.Headings))
	for _, h := range d.Headings {
		if h.Offset < 0 || h.Offset >= int64(len(d.Text)) || !utf8.RuneStart(d.Text[h.Offset]) {
			continue
		}
		hs = append(hs, h)
	}
	sort.SliceStable(hs, func(i, j int) bool { return hs[i].Offset < hs[j].Offset })

	var (
		out   []section
		stack []Heading
		start int
		path  string
	)
	for _, h := range hs {
		if int(h.Offset) > start {
			out = append(out, section{span{start, int(h.Offset)}, path})
		}
		start = int(h.Offset)
		for len(stack) > 0 && stack[len(stack)-1].Level >= h.Level {
			stack = stack[:len(stack)-1]
		}
		stack = append(stack, h)
		path = headingPath(stack)
	}
	if start < len(d.Text) {
		out = append(out, section{span{start, len(d.Text)}, path})
	}
	return out
}

// headingPath renders a heading stack as "A > B > C".
func headingPath(stack []Heading) string {
	titles := make([]string, len(stack))
	for i, h := range stack {
		titles[i] = h.Title
	}
	return strings.Join(titles, " > ")
}

// windows cuts text[start:end] into spans of at most window runes. Each
// span after the first starts overlap runes before the previous one ended.
// A span that would not reach end is shortened to the best boundary found
// in its second half, see cutPoint.
func windows(text string, start, end, window, overlap int, code bool) []span {
	var out []span
	for start < end {
		half := advance(text, start, end, window/2)
		limit := advance(text, half, end, window-window/2)
		cut := limit
		if limit < end {
			cut = cutPoint(text, half, limit, code)
		}
		out = append(out, span{start, cut})
		if cut >= end {
			break
		}
		next := retreat(text, cut, start, overlap)
		if next <= start {
			next = cut
		}
		start = next
	}
	return out
}

// advance returns the byte offset n runes after off, capped at end.
func advance(text string, off, end, n int) int {
	for ; n > 0 && off < end; n-- {
		_, size := utf8.DecodeRuneInString(text[off:end])
		off += size
	}
	return off
}

// retreat returns the byte offset n runes before off, floored at floor.
func retreat(text string, off, floor, n int) int {
	for ; n > 0 && off > floor; n-- {
		_, size := utf8.DecodeLastRuneInString(text[floor:off])
		off -= size
	}
	return off
}

// cutPoint picks where a window that must stop by limit should end, looking
// backwards from limit no further than lo. Boundaries are tried from
// strongest to weakest and the last one of the strongest tier wins: a
// paragraph break or sentence end (a blank line alone for code), then a
// line end, then any whitespace. Without one, the window is cut at limit.
func cutPoint(text string, lo, limit int, code bool) int {
	best := [3]int{-1, -1, -1}
	for i := limit; i > lo; {
		_, size := utf8.DecodeLastRuneInString(text[:i])
		if size == 0 {
			break
		}
		tier := boundaryTier(text, i-size, i, code)
		if tier >= 0 && best[tier] < 0 {
			best[tier] = i
			if tier == 0 {
				break
			}
		}
		i -= size
	}
	for _, b := range best {
		if b >= 0 {
			return b
		}
	}
	return limit
}

// boundaryTier classifies the rune text[at:next] as a place to end a chunk
// right after it: 0 for a paragraph break or sentence end, 1 for a line
// end, 2 for other whitespace, -1 for none. For code only blank lines and
// line ends count, since periods and question marks carry no sentence
// meaning there.
func boundaryTier(text string, at, next int, code bool) int {
	switch text[at] {
	case '\n':
		if blankLineEndsAt(text, at) {
			return 0
		}
		return 1
	case '.', '!', '?':
		// ASCII terminators are sentence ends only before whitespace, so
		// "3.14" and "example.com" stay whole.
		if code {
			return -1
		}
		if next >= len(text) || isSpaceAt(text, next) {
			return 0
		}
		return -1
	}
	r, _ := utf8.DecodeRuneInString(text[at:next])
	switch r {
	case '。', '！', '？':
		if code {
			return -1
		}
		return 0
	}
	if unicode.IsSpace(r) {
		return 2
	}
	return -1
}

// blankLineEndsAt reports whether the '\n' at nl closes an empty line, i.e.
// the text before it ends in another line end (LF or CRLF).
func blankLineEndsAt(text string, nl int) bool {
	i := nl
	if i > 0 && text[i-1] == '\r' {
		i--
	}
	return i > 0 && text[i-1] == '\n'
}

// isSpaceAt reports whether the rune starting at off is white space.
func isSpaceAt(text string, off int) bool {
	r, _ := utf8.DecodeRuneInString(text[off:])
	return unicode.IsSpace(r)
}
