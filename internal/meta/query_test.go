package meta

import (
	"reflect"
	"testing"
	"time"
)

func mustParse(t *testing.T, raw string) Filter {
	t.Helper()
	f, err := ParseQuery(raw)
	if err != nil {
		t.Fatalf("%q: %v", raw, err)
	}
	return f
}

// unset applies the -1 convention to a literal the test writes with zero
// sizes meaning "not given".
func unset(f Filter) Filter {
	if f.MinSize == 0 {
		f.MinSize = -1
	}
	if f.MaxSize == 0 {
		f.MaxSize = -1
	}
	return f
}

func TestParseQueryGrammar(t *testing.T) {
	loc := time.Local
	for raw, want := range map[string]Filter{
		"report":                    {Terms: []Term{{Text: "report"}}},
		"my report":                 {Terms: []Term{{Text: "my"}, {Text: "report"}}},
		`"my report"`:               {Terms: []Term{{Text: "my report"}}},
		`-"my report" x`:            {Terms: []Term{{Text: "my report", Negate: true}, {Text: "x"}}},
		`a"b.txt`:                   {Terms: []Term{{Text: `a"b.txt`}}},
		"*.md":                      {Terms: []Term{{Text: "*.md", Glob: true}}},
		"src/handler":               {Paths: []string{"src/handler"}},
		"path:src":                  {Paths: []string{"src"}},
		"-path:vendor -a/b":         {NotPaths: []string{"vendor", "a/b"}},
		"ext:Go,md":                 {Ext: []string{"go", "md"}},
		"ext:.go":                   {Ext: []string{"go"}},
		"size:>1m":                  {MinSize: 1<<20 + 1},
		"size:>=1m":                 {MinSize: 1 << 20},
		"size:<100k":                {MaxSize: 100<<10 - 1},
		"size:<=100kb":              {MaxSize: 100 << 10},
		"size:1m..10m":              {MinSize: 1 << 20, MaxSize: 10 << 20},
		"size:4096":                 {MinSize: 4096, MaxSize: 4096},
		"size:1.5k":                 {MinSize: 1536, MaxSize: 1536},
		"dm:>2026-09-01":            {ModifiedAfter: time.Date(2026, 9, 2, 0, 0, 0, 0, loc)},
		"dm:>2026-09":               {ModifiedAfter: time.Date(2026, 10, 1, 0, 0, 0, 0, loc)},
		"dm:>2026":                  {ModifiedAfter: time.Date(2027, 1, 1, 0, 0, 0, 0, loc)},
		"dm:<2026-09":               {ModifiedBefore: time.Date(2026, 9, 1, 0, 0, 0, 0, loc)},
		"dm:2026-09":                {ModifiedAfter: time.Date(2026, 9, 1, 0, 0, 0, 0, loc), ModifiedBefore: time.Date(2026, 10, 1, 0, 0, 0, 0, loc)},
		"dm:2026-09-01..2026-09-30": {ModifiedAfter: time.Date(2026, 9, 1, 0, 0, 0, 0, loc), ModifiedBefore: time.Date(2026, 10, 1, 0, 0, 0, 0, loc)},
		"type:dir":                  {Kind: "dir"},
		"-tmp -ext:bak type:file":   {Terms: []Term{{Text: "tmp", Negate: true}}, NotExt: []string{"bak"}, Kind: "file"},
		"12:30 c:":                  {Terms: []Term{{Text: "12:30"}, {Text: "c:"}}},
		"  ":                        {},
	} {
		got, err := ParseQuery(raw)
		if err != nil {
			t.Fatalf("%q: %v", raw, err)
		}
		if !reflect.DeepEqual(got, unset(want)) {
			t.Errorf("%q:\n got %+v\nwant %+v", raw, got, unset(want))
		}
	}
	if !mustParse(t, "").Empty() || mustParse(t, "type:dir").Empty() || mustParse(t, "size:<1").Empty() {
		t.Fatal("Empty must reflect whether anything constrains the query")
	}
}

func TestParseQueryRejectsBadValues(t *testing.T) {
	for _, raw := range []string{
		"size:lots", "size:>", "size:10m..1m", "size:<0", "-size:1k", "size:99999999999999999999g", "dm:yesterday", "dm:2026-13",
		"dm:2026-10..2026-09", "type:link", "-type:dir", "ext:", "ext:a/b", "ext:go,", "path:",
		"size:>1m size:<2m size:3m", "type:dir type:file", "a\x00b", string([]byte{0xff}),
	} {
		if _, err := ParseQuery(raw); err == nil {
			t.Errorf("%q accepted", raw)
		}
	}
	if f, err := ParseQuery(`"unterminated`); err != nil || len(f.Terms) != 1 || f.Terms[0].Text != `"unterminated` {
		t.Fatalf("an unterminated quote is a literal word: %+v %v", f, err)
	}
}

func TestGlobAnchorUsesTheLongestLiteralRun(t *testing.T) {
	for raw, want := range map[string]struct {
		anchor string
		short  bool
	}{
		"*.md": {".md", false}, "rep?rt*2026": {"2026", false}, "a*b": {"a", true}, "*": {"", false},
		"-tmp *.go": {".go", false}, "ext:go": {".go", false}, "ext:go,md": {"", false}, "size:>1k": {"", false},
		"ab report": {"report", false}, "REPORT": {"report", false}, "src/handler": {"handler", false},
		"-path:vendor": {"", false}, `"my report"`: {"my report", false},
	} {
		f := mustParse(t, raw)
		anchor, short := f.anchor()
		if anchor != want.anchor || short != want.short {
			t.Errorf("%q: anchor %q short %v, want %+v", raw, anchor, short, want)
		}
	}
	// A name anchor never expands subtrees; a path anchor and no anchor do.
	for raw, expand := range map[string]bool{"report": false, "*.md": false, "ext:go": false, "src/handler": true, "path:a": true, "size:>1k": true, "report path:a": false, "re path:handler": true} {
		stmt, _ := searchSQL(SearchQuery{Filter: mustParse(t, raw), Limit: 10}, 100)
		if got := containsExpanded(stmt); got != expand {
			t.Errorf("%q: expanded=%v, want %v", raw, got, expand)
		}
	}
}

func containsExpanded(stmt string) bool {
	for i := 0; i+8 <= len(stmt); i++ {
		if stmt[i:i+8] == "expanded" {
			return true
		}
	}
	return false
}
