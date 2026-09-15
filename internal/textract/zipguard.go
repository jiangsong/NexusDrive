package textract

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"sort"
	"strings"
)

// guardedZip is an archive reader that enforces the Options archive limits
// (DESIGN.md §9): the entry count when the archive is opened, and the
// uncompressed size of every entry as well as the running total across all
// opened entries while they are read. Sizes are checked twice, against the
// header's declared value before any inflation starts and against the bytes
// actually delivered, so a header that lies about its size cannot bypass
// the limit. A guardedZip is used by one extractor sequentially and is not
// safe for concurrent use.
type guardedZip struct {
	zr     *zip.Reader
	opt    Options
	total  int64 // uncompressed bytes delivered so far across all entries
	byName map[string]*zip.File
}

// openZip parses the archive directory and rejects archives with more than
// opt.MaxZipEntries entries before anything is inflated.
func openZip(r io.ReaderAt, size int64, opt Options) (*guardedZip, error) {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return nil, fmt.Errorf("textract: %w", err)
	}
	if opt.MaxZipEntries > 0 && len(zr.File) > opt.MaxZipEntries {
		return nil, fmt.Errorf("%w: %d entries", ErrZipBomb, len(zr.File))
	}
	g := &guardedZip{zr: zr, opt: opt, byName: make(map[string]*zip.File, len(zr.File))}
	for _, f := range zr.File {
		name := strings.TrimPrefix(f.Name, "/")
		if _, dup := g.byName[name]; !dup {
			g.byName[name] = f
		}
	}
	return g, nil
}

// Open returns the entry's uncompressed contents. It fails with fs.ErrNotExist
// for an unknown name and with ErrZipBomb when the declared size alone
// already breaks a limit; the returned reader keeps enforcing the limits on
// the bytes it delivers.
func (g *guardedZip) Open(name string) (io.ReadCloser, error) {
	f, ok := g.byName[name]
	if !ok {
		return nil, fmt.Errorf("textract: %s: %w", name, fs.ErrNotExist)
	}
	declared := int64(f.UncompressedSize64)
	if declared < 0 || g.opt.MaxZipEntryBytes > 0 && declared > g.opt.MaxZipEntryBytes {
		return nil, fmt.Errorf("%w: %s declares %d bytes", ErrZipBomb, name, f.UncompressedSize64)
	}
	if g.opt.MaxZipTotalBytes > 0 && g.total+declared > g.opt.MaxZipTotalBytes {
		return nil, fmt.Errorf("%w: %s would exceed the archive total", ErrZipBomb, name)
	}
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("textract: %s: %w", name, err)
	}
	return &guardReader{rc: rc, g: g, name: name, declared: declared}, nil
}

// Names lists the entries starting with prefix in lexical order.
func (g *guardedZip) Names(prefix string) []string {
	var out []string
	for name := range g.byName {
		if strings.HasPrefix(name, prefix) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// guardReader counts the bytes an entry actually delivers and turns a limit
// breach into a sticky ErrZipBomb.
type guardReader struct {
	rc       io.ReadCloser
	g        *guardedZip
	name     string
	declared int64
	n        int64 // bytes delivered from this entry
	err      error
}

func (r *guardReader) Read(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	n, err := r.rc.Read(p)
	r.n += int64(n)
	r.g.total += int64(n)
	switch {
	case r.g.opt.MaxZipEntryBytes > 0 && r.n > r.g.opt.MaxZipEntryBytes:
		r.err = fmt.Errorf("%w: %s", ErrZipBomb, r.name)
	case r.g.opt.MaxZipTotalBytes > 0 && r.g.total > r.g.opt.MaxZipTotalBytes:
		r.err = fmt.Errorf("%w: %s exceeds the archive total", ErrZipBomb, r.name)
	case errors.Is(err, zip.ErrFormat):
		// While reading an entry archive/zip only reports ErrFormat when the
		// stream disagrees with the header, typically because it inflates
		// past the declared size: the signature of a lying header.
		r.err = fmt.Errorf("%w: %s does not match its declared %d bytes", ErrZipBomb, r.name, r.declared)
	}
	if r.err != nil {
		return 0, r.err
	}
	return n, err
}

func (r *guardReader) Close() error { return r.rc.Close() }
