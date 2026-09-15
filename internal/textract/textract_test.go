package textract

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestKindOfUsesExtensionThenSniffs(t *testing.T) {
	for _, tc := range []struct {
		name string
		head []byte
		want Kind
	}{
		{"a.md", []byte("# x"), KindMarkdown},
		{"a.go", []byte("package x"), KindCode},
		{"a.txt", []byte("hello"), KindText},
		{"a.docx", []byte("PK\x03\x04"), KindDocx},
		{"a.pdf", []byte("%PDF-1.4"), KindPDF},
		{"a.txt", []byte("\x89PNG\r\n\x1a\n"), KindUnsupported},
		{"a.mkv", []byte("\x1aE\xdf\xa3"), KindUnsupported},
		{"noext", []byte("plain words"), KindText},
		{"A.MD", []byte("# upper"), KindMarkdown},
		{"dir/x.XLSX", []byte("PK\x03\x04"), KindXlsx},
		{"a.pptx", []byte("PK\x03\x04"), KindPptx},
		{"a.docx", []byte("not a zip"), KindUnsupported},
		{"a.pdf", []byte("PK\x03\x04"), KindUnsupported},
		{"a.json", []byte("{}"), KindText},
		{"a.txt", []byte("\xff\xd8\xff\xe0jfif"), KindUnsupported},
		{"a.txt", []byte("\x1f\x8b\x08"), KindUnsupported},
		{"a.txt", []byte("PK\x03\x04"), KindUnsupported},
		{"noext", []byte("bin\x00\x00\x00\x00"), KindUnsupported},
		{"noext", []byte("caf\xe9 au lait"), KindUnsupported},
		{"noext", []byte("caf\xe9"), KindText}, // a head cut inside a rune is not evidence of binary
		{"noext", []byte("\xe4\xb8\xad\xe6\x96"), KindText},
		{"noext", nil, KindText},
		{"a.txt", nil, KindText},
		{"a.exe", []byte("MZ"), KindUnsupported},
		{"Makefile", []byte("all:\n\tgo build\n"), KindText},
	} {
		if got := KindOf(tc.name, tc.head); got != tc.want {
			t.Errorf("KindOf(%q, %q) = %q want %q", tc.name, tc.head, got, tc.want)
		}
	}
}

func TestExtractHonoursContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Extract(ctx, KindText, strings.NewReader("x"), 1, DefaultOptions()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestExtractRejectsUnsupportedKinds(t *testing.T) {
	for _, kind := range []Kind{KindUnsupported, Kind("bogus")} {
		if _, err := Extract(context.Background(), kind, strings.NewReader("x"), 1, DefaultOptions()); !errors.Is(err, ErrUnsupported) {
			t.Errorf("kind %q: %v", kind, err)
		}
	}
}

func TestDefaultOptionsMatchTheDesign(t *testing.T) {
	opt := DefaultOptions()
	if opt.MaxTextBytes != 2<<20 || opt.Timeout.Seconds() != 30 || opt.MaxZipEntries != 4096 ||
		opt.MaxZipEntryBytes != 64<<20 || opt.MaxZipTotalBytes != 256<<20 || opt.MaxSheetRows != 5000 {
		t.Fatalf("%+v", opt)
	}
}
