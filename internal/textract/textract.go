// Package textract turns files the index cares about into plain text plus a
// heading outline, without depending on the rest of CloudFS (DESIGN.md §9,
// T-37). Callers classify a file with KindOf and hand the bytes to Extract;
// the returned Doc is what the chunker (chunk.go) and the index consume.
//
// The package is deliberately standard-library only so that a malformed file
// can at worst cost CPU inside the limits in Options, never a crash of the
// daemon: every extractor is bounded by MaxTextBytes, the archive limits and
// the context deadline derived from Options.Timeout.
package textract

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"
	"unicode/utf8"
)

// Kind is the extraction strategy chosen for a file. It is stored alongside
// each indexed document, so the string values are part of the index schema.
type Kind string

const (
	KindText        Kind = "text"
	KindMarkdown    Kind = "markdown"
	KindCode        Kind = "code"
	KindDocx        Kind = "docx"
	KindXlsx        Kind = "xlsx"
	KindPptx        Kind = "pptx"
	KindPDF         Kind = "pdf"
	KindUnsupported Kind = ""
)

// ExtractorVer is bumped whenever the text produced for an unchanged file
// would differ, so the indexer knows to re-extract documents built by an
// older build.
const ExtractorVer = 1

// Heading is one outline entry of a Doc. Offset points at the first byte of
// the heading line inside Doc.Text; the chunker uses the outline to label
// chunks with their enclosing section.
type Heading struct {
	Level  int
	Title  string
	Offset int64 // byte offset of the heading line in Doc.Text
}

// Doc is the extracted form of one file.
type Doc struct {
	Text      string
	Headings  []Heading
	Truncated bool
	// OffsetKind says what byte offsets into Text mean: "file" when Text is
	// the file's own bytes (text kinds keep line endings and length, so an
	// offset can be handed straight to a ranged read), "text" when Text was
	// synthesised from a container format and offsets only address Text.
	OffsetKind string
}

// Options bounds every extractor. The zero value is not usable; start from
// DefaultOptions.
type Options struct {
	MaxTextBytes     int64         // extracted text is cut on a rune boundary past this
	Timeout          time.Duration // applied to the context handed to Extract when > 0
	MaxZipEntries    int           // office containers with more entries are rejected
	MaxZipEntryBytes int64         // uncompressed size allowed for any one archive entry
	MaxZipTotalBytes int64         // uncompressed bytes allowed per archive in total
	MaxSheetRows     int           // rows kept per spreadsheet sheet before truncating
}

// DefaultOptions returns the limits from DESIGN.md §9.
func DefaultOptions() Options {
	return Options{
		MaxTextBytes:     2 << 20,
		Timeout:          30 * time.Second,
		MaxZipEntries:    4096,
		MaxZipEntryBytes: 64 << 20,
		MaxZipTotalBytes: 256 << 20,
		MaxSheetRows:     5000,
	}
}

var (
	ErrNotUTF8     = errors.New("textract: not valid UTF-8")
	ErrUnsupported = errors.New("textract: unsupported file type")
	ErrZipBomb     = errors.New("textract: archive exceeds extraction limits")
	ErrNoText      = errors.New("textract: no extractable text")
	ErrGarbled     = errors.New("textract: extracted text is unreadable")
)

// kindByExt maps a lower-cased extension to the kind the extension promises.
// Office and PDF kinds are only trusted when the file head carries the
// matching magic, see KindOf.
var kindByExt = map[string]Kind{
	".md": KindMarkdown, ".markdown": KindMarkdown,

	".txt": KindText, ".rst": KindText, ".csv": KindText, ".json": KindText,
	".yaml": KindText, ".yml": KindText, ".toml": KindText, ".html": KindText,
	".htm": KindText,

	".go": KindCode, ".py": KindCode, ".ts": KindCode, ".js": KindCode,
	".rs": KindCode, ".java": KindCode, ".c": KindCode, ".h": KindCode,
	".sh": KindCode, ".sql": KindCode,

	".docx": KindDocx, ".xlsx": KindXlsx, ".pptx": KindPptx, ".pdf": KindPDF,
}

// binaryMagic lists leading bytes that identify well-known binary formats.
// A file that carries one of these is never treated as text, whatever its
// extension says.
var binaryMagic = [][]byte{
	[]byte("\x89PNG"),       // PNG
	[]byte("\xff\xd8\xff"),  // JPEG
	[]byte("PK\x03\x04"),    // ZIP (and every OOXML container)
	[]byte("\x1aE\xdf\xa3"), // Matroska / WebM
	[]byte("\x1f\x8b"),      // gzip
	[]byte("\x7fELF"),       // ELF executables
	[]byte("%PDF-"),         // PDF renamed to a text extension
}

var (
	zipMagic = []byte("PK\x03\x04")
	pdfMagic = []byte("%PDF-")
)

// KindOf classifies a file from its name and the first bytes of its content.
// The extension decides the candidate kind; head is then used to veto it
// (an office extension without a zip header, a .txt holding a PNG) or, for
// files without an extension, to admit plain UTF-8 text. head may be any
// prefix of the file; a UTF-8 sequence cut short at its end is tolerated.
func KindOf(name string, head []byte) Kind {
	ext := strings.ToLower(path.Ext(name))
	kind, known := kindByExt[ext]
	if !known && ext != "" {
		return KindUnsupported
	}
	switch kind {
	case KindDocx, KindXlsx, KindPptx:
		if !bytes.HasPrefix(head, zipMagic) {
			return KindUnsupported
		}
		return kind
	case KindPDF:
		if !bytes.HasPrefix(head, pdfMagic) {
			return KindUnsupported
		}
		return kind
	case KindText, KindMarkdown, KindCode:
		if looksBinary(head) {
			return KindUnsupported
		}
		return kind
	}
	// No extension: admit only what is demonstrably text.
	if looksBinary(head) || bytes.IndexByte(head, 0) >= 0 || !validUTF8Prefix(head) {
		return KindUnsupported
	}
	return KindText
}

// looksBinary reports whether head starts with a known binary magic or has
// more than 1% NUL bytes, the usual tell of a binary format.
func looksBinary(head []byte) bool {
	for _, m := range binaryMagic {
		if bytes.HasPrefix(head, m) {
			return true
		}
	}
	return bytes.Count(head, []byte{0})*100 > len(head)
}

// validUTF8Prefix is utf8.Valid that forgives a multi-byte sequence the
// caller's head buffer cut in the middle.
func validUTF8Prefix(b []byte) bool {
	if utf8.Valid(b) {
		return true
	}
	// Find the start of the last rune; it must be an incomplete but so far
	// well-formed lead + continuation bytes, and everything before it valid.
	i := len(b) - 1
	for i >= 0 && i > len(b)-utf8.UTFMax && !utf8.RuneStart(b[i]) {
		i--
	}
	if i < 0 || !utf8.Valid(b[:i]) {
		return false
	}
	tail := b[i:]
	if utf8.FullRune(tail) {
		return false // complete and therefore genuinely invalid
	}
	for _, c := range tail[1:] {
		if c&0xC0 != 0x80 {
			return false
		}
	}
	return tail[0] >= 0xC2 && tail[0] <= 0xF4
}

// Extract produces the Doc for a file of the given kind. size is the file
// length; r must serve at least that many bytes. Errors are one of the
// package sentinels (wrapped), a context error, or an I/O error from r.
func Extract(ctx context.Context, kind Kind, r io.ReaderAt, size int64, opt Options) (Doc, error) {
	if err := ctx.Err(); err != nil {
		return Doc{}, err
	}
	if opt.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opt.Timeout)
		defer cancel()
	}
	switch kind {
	case KindText, KindCode:
		return extractText(ctx, r, size, opt, false)
	case KindMarkdown:
		return extractText(ctx, r, size, opt, true)
	case KindDocx, KindXlsx, KindPptx, KindPDF:
		// Container formats land in office.go / pdf.go.
		return Doc{}, fmt.Errorf("%w: %s extraction is not built in", ErrUnsupported, kind)
	default:
		return Doc{}, ErrUnsupported
	}
}
