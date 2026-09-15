package textract

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"unicode/utf8"
)

// extractText handles the kinds whose Text is the file itself. Bytes are kept
// exactly as read (CRLF included) so every offset into Doc.Text is also a
// file offset; only NUL bytes are replaced by spaces, which keeps the length.
// When markdown is set, ATX headings outside fenced code blocks fill
// Doc.Headings.
func extractText(ctx context.Context, r io.ReaderAt, size int64, opt Options, markdown bool) (Doc, error) {
	if size < 0 {
		size = 0
	}
	limit := size
	truncated := false
	if opt.MaxTextBytes > 0 && size > opt.MaxTextBytes {
		limit = opt.MaxTextBytes
		truncated = true
	}
	buf := make([]byte, limit)
	n, err := io.ReadFull(io.NewSectionReader(r, 0, limit), buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return Doc{}, err
	}
	buf = buf[:n]
	if err := ctx.Err(); err != nil {
		return Doc{}, err
	}
	if truncated {
		buf = buf[:lastRuneBoundary(buf)]
	}
	if !utf8.Valid(buf) {
		return Doc{}, ErrNotUTF8
	}
	if len(bytes.TrimSpace(buf)) == 0 {
		return Doc{}, ErrNoText
	}
	buf = bytes.ReplaceAll(buf, []byte{0}, []byte{' '})
	d := Doc{Text: string(buf), Truncated: truncated, OffsetKind: "file"}
	if markdown {
		d.Headings = markdownHeadings(d.Text)
	}
	return d, nil
}

// lastRuneBoundary returns the length of the longest prefix of b that does
// not end in a partial UTF-8 sequence.
func lastRuneBoundary(b []byte) int {
	i := len(b) - 1
	for i >= 0 && i > len(b)-utf8.UTFMax && !utf8.RuneStart(b[i]) {
		i--
	}
	if i < 0 {
		return len(b)
	}
	if utf8.FullRune(b[i:]) {
		return len(b)
	}
	return i
}

// markdownHeadings scans ATX headings ("# Title" through "###### Title")
// that sit outside fenced code blocks. Offsets point at the start of the
// heading line, matching CommonMark's allowance of up to three leading
// spaces.
func markdownHeadings(text string) []Heading {
	var (
		out       []Heading
		off       int64
		fenceChar byte
		fenceLen  int
	)
	for len(text) > 0 {
		line := text
		next := len(text)
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			line, next = text[:i], i+1
		}
		line = strings.TrimRight(line, "\r")
		body := strings.TrimLeft(line, " ")
		if len(line)-len(body) <= 3 {
			if fenceChar != 0 {
				if n := fenceRun(body, fenceChar); n >= fenceLen && strings.TrimSpace(body[n:]) == "" {
					fenceChar = 0
				}
			} else if n := fenceRun(body, '`'); n >= 3 {
				fenceChar, fenceLen = '`', n
			} else if n := fenceRun(body, '~'); n >= 3 {
				fenceChar, fenceLen = '~', n
			} else if h, ok := atxHeading(body); ok {
				h.Offset = off
				out = append(out, h)
			}
		}
		off += int64(next)
		text = text[next:]
	}
	return out
}

// fenceRun counts the leading run of c in s.
func fenceRun(s string, c byte) int {
	n := 0
	for n < len(s) && s[n] == c {
		n++
	}
	return n
}

// atxHeading parses "#{1,6} title [#*]" and rejects empty headings.
func atxHeading(s string) (Heading, bool) {
	level := fenceRun(s, '#')
	if level == 0 || level > 6 {
		return Heading{}, false
	}
	rest := s[level:]
	if rest != "" && rest[0] != ' ' && rest[0] != '\t' {
		return Heading{}, false
	}
	title := strings.TrimSpace(rest)
	// An optional closing sequence of #s counts only when a space precedes
	// it ("# foo ##" is "foo", "# foo#" keeps its #, "# ###" is empty).
	if open := strings.TrimRight(title, "#"); len(open) < len(title) {
		if open == "" {
			title = ""
		} else if c := open[len(open)-1]; c == ' ' || c == '\t' {
			title = strings.TrimSpace(open)
		}
	}
	if title == "" {
		return Heading{}, false
	}
	return Heading{Level: level, Title: title}, true
}
