package meta

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// The query grammar is Everything's: bare words are AND-ed substrings of the
// name, quotes group a literal with spaces, `*` and `?` are wildcards over
// the whole name, and key:value terms filter on extension, size, modified
// time, kind and path. A leading `-` negates a word, an ext: or a path:.
// The console's search_query.js implements the same table, so a query the
// filter bar builds and a query a person types mean the same thing here.
//
//	report            substring of the folded name
//	"my report"       literal with a space
//	*.md  rep?rt*     GLOB over the folded name
//	src/handler       path substring (a bare word with a slash)
//	path:src          path substring
//	ext:go,md         extension, comma = OR
//	size:>1m          bytes; k/m/g binary; > and < strict; a..b inclusive
//	dm:>2026-09-01    modified; YYYY, YYYY-MM or YYYY-MM-DD, local time
//	type:dir|file     kind
//	-term -ext:bak    negation

// Term is one name term: a substring, or a GLOB pattern when Glob is set.
type Term struct {
	Text   string
	Glob   bool
	Negate bool
}

// Filter is a parsed query. Sizes use -1 for "unset" because 0 is a real
// bound (size:<1 is "empty files"), so a Filter built by hand must set both
// or start from ParseQuery.
type Filter struct {
	// Terms are bare words: substrings of the folded name, or GLOB patterns.
	Terms []Term
	// Paths are path: terms and bare words containing "/": folded path
	// substrings. Any positive path expands anchor subtrees, as the old
	// path query did.
	Paths    []string
	NotPaths []string
	// Ext and NotExt are lower-case without the dot.
	Ext, NotExt []string
	// MinSize and MaxSize are inclusive; -1 means unset.
	MinSize int64
	MaxSize int64
	// ModifiedAfter is inclusive and ModifiedBefore exclusive; zero = unset.
	ModifiedAfter  time.Time
	ModifiedBefore time.Time
	// Kind is "", "dir" or "file".
	Kind string
}

// Empty reports whether the filter constrains nothing.
func (f Filter) Empty() bool {
	return len(f.Terms) == 0 && len(f.Paths) == 0 && len(f.NotPaths) == 0 &&
		len(f.Ext) == 0 && len(f.NotExt) == 0 && f.MinSize < 0 && f.MaxSize < 0 &&
		f.ModifiedAfter.IsZero() && f.ModifiedBefore.IsZero() && f.Kind == ""
}

// SearchQuery is what Find answers.
type SearchQuery struct {
	Filter Filter
	// Roots restricts results to these subtrees; nil is unrestricted and an
	// empty non-nil slice permits nothing, the SearchWithin contract.
	Roots []string
	Limit int
	// Sort is "" (depth, path, ino), name, size, mtime or path; a leading
	// "-" reverses it. It orders the collected set only: when the answer
	// is not Complete, "largest N" is the largest of what the budget
	// reached, which the caller must say.
	Sort string
}

// ParseQuery parses the grammar above. An unterminated quote is a literal
// word, unknown key:value words are bare words, and a value the grammar
// cannot read is an error rather than a silently ignored filter.
func ParseQuery(raw string) (Filter, error) {
	f := Filter{MinSize: -1, MaxSize: -1}
	if !utf8.ValidString(raw) || len(raw) > 4096 || strings.ContainsRune(raw, 0) {
		return f, errors.New("meta: invalid search query")
	}
	var seenSize, seenDate, seenKind bool
	for _, word := range splitQuery(raw) {
		negate := word.negate
		if !word.quoted && strings.HasPrefix(word.text, "-") && len(word.text) > 1 {
			negate, word.text = true, word.text[1:]
		}
		key, value, isKeyed := "", word.text, false
		if !word.quoted {
			if i := strings.IndexByte(word.text, ':'); i > 0 {
				switch word.text[:i] {
				case "ext", "size", "dm", "type", "path":
					key, value, isKeyed = word.text[:i], word.text[i+1:], true
				}
			}
		}
		if !isKeyed {
			if value == "" {
				// "" matches everything and says nothing.
				continue
			}
			if strings.Contains(value, "/") {
				if negate {
					f.NotPaths = append(f.NotPaths, value)
				} else {
					f.Paths = append(f.Paths, value)
				}
				continue
			}
			f.Terms = append(f.Terms, Term{Text: value, Glob: strings.ContainsAny(value, "*?"), Negate: negate})
			continue
		}
		if value == "" {
			return f, fmt.Errorf("meta: %s: needs a value", key)
		}
		switch key {
		case "path":
			if negate {
				f.NotPaths = append(f.NotPaths, value)
			} else {
				f.Paths = append(f.Paths, value)
			}
		case "ext":
			for _, ext := range strings.Split(strings.ToLower(value), ",") {
				ext = strings.TrimPrefix(ext, ".")
				if ext == "" || strings.ContainsAny(ext, "/.") {
					return f, fmt.Errorf("meta: ext: %q is not an extension", value)
				}
				if negate {
					f.NotExt = append(f.NotExt, ext)
				} else {
					f.Ext = append(f.Ext, ext)
				}
			}
		case "size":
			if negate || seenSize {
				return f, errors.New("meta: size: one positive size range per query")
			}
			seenSize = true
			lo, hi, err := parseRange(value, parseSize)
			if err != nil {
				return f, fmt.Errorf("meta: size: %w", err)
			}
			f.MinSize, f.MaxSize = lo, hi
		case "dm":
			if negate || seenDate {
				return f, errors.New("meta: dm: one positive date range per query")
			}
			seenDate = true
			lo, hi, err := parseDateRange(value)
			if err != nil {
				return f, fmt.Errorf("meta: dm: %w", err)
			}
			f.ModifiedAfter, f.ModifiedBefore = lo, hi
		case "type":
			if negate || seenKind {
				return f, errors.New("meta: type: one positive kind per query")
			}
			seenKind = true
			switch value {
			case "dir", "file":
				f.Kind = value
			default:
				return f, fmt.Errorf("meta: type: %q is not dir or file", value)
			}
		}
	}
	return f, nil
}

type queryWord struct {
	text   string
	quoted bool
	negate bool
}

// splitQuery splits on whitespace. A word that starts with a double quote
// (or with -" for a negated one) runs to the closing quote, whitespace
// included; without one, the quote is part of a literal word. Quotes inside
// a word are literal, so a"b.txt is still searchable.
func splitQuery(raw string) []queryWord {
	var words []queryWord
	runes := []rune(raw)
	for i := 0; i < len(runes); {
		if unicode.IsSpace(runes[i]) {
			i++
			continue
		}
		start := i
		open := i
		if runes[i] == '-' && i+1 < len(runes) && runes[i+1] == '"' {
			open = i + 1
		}
		if runes[open] == '"' {
			if end := indexRune(runes, open+1, '"'); end >= 0 {
				// A closing quote ends the word; anything glued on after it
				// starts a new one.
				words = append(words, queryWord{text: string(runes[open+1 : end]), quoted: true, negate: open > start})
				i = end + 1
				continue
			}
		}
		for i < len(runes) && !unicode.IsSpace(runes[i]) {
			i++
		}
		words = append(words, queryWord{text: string(runes[start:i])})
	}
	return words
}

func indexRune(runes []rune, from int, r rune) int {
	for i := from; i < len(runes); i++ {
		if runes[i] == r {
			return i
		}
	}
	return -1
}

// parseRange reads >v, >=v, <v, <=v, a..b or v with the given value parser.
// > and < are strict; a..b and a bare value are inclusive.
func parseRange(value string, parse func(string) (int64, error)) (lo, hi int64, err error) {
	lo, hi = -1, -1
	switch {
	case strings.HasPrefix(value, ">="):
		lo, err = parse(value[2:])
	case strings.HasPrefix(value, "<="):
		hi, err = parse(value[2:])
	case strings.HasPrefix(value, ">"):
		if lo, err = parse(value[1:]); err == nil {
			lo++
		}
	case strings.HasPrefix(value, "<"):
		if hi, err = parse(value[1:]); err == nil {
			hi--
			if hi < 0 {
				return 0, 0, fmt.Errorf("%q excludes everything", value)
			}
		}
	case strings.Contains(value, ".."):
		a, b, _ := strings.Cut(value, "..")
		if lo, err = parse(a); err != nil {
			return 0, 0, err
		}
		if hi, err = parse(b); err != nil {
			return 0, 0, err
		}
		if lo > hi {
			return 0, 0, fmt.Errorf("%q is an empty range", value)
		}
	default:
		if lo, err = parse(value); err == nil {
			hi = lo
		}
	}
	return lo, hi, err
}

// parseSize reads a byte count with an optional k/m/g suffix (kb/mb/gb
// accepted); the units are binary, as file managers show them.
func parseSize(s string) (int64, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return 0, errors.New("missing size")
	}
	mult := int64(1)
	for _, unit := range []struct {
		suffix string
		mult   int64
	}{{"kb", 1 << 10}, {"mb", 1 << 20}, {"gb", 1 << 30}, {"k", 1 << 10}, {"m", 1 << 20}, {"g", 1 << 30}, {"b", 1}} {
		if strings.HasSuffix(s, unit.suffix) {
			s, mult = strings.TrimSuffix(s, unit.suffix), unit.mult
			break
		}
	}
	if strings.Contains(s, ".") {
		v, err := strconv.ParseFloat(s, 64)
		if err != nil || v < 0 || v > float64(math.MaxInt64/mult) {
			return 0, fmt.Errorf("%q is not a size", s)
		}
		return int64(v * float64(mult)), nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v < 0 || v > math.MaxInt64/mult {
		return 0, fmt.Errorf("%q is not a size", s)
	}
	return v * mult, nil
}

// datePeriod is a calendar period a dm: value names: its first instant and
// the first instant after it, in local time.
type datePeriod struct{ start, end time.Time }

func parseDate(s string) (datePeriod, error) {
	loc := time.Local
	for _, layout := range []string{"2006-01-02", "2006-01", "2006"} {
		t, err := time.ParseInLocation(layout, s, loc)
		if err != nil {
			continue
		}
		y, m, d := t.Date()
		switch layout {
		case "2006-01-02":
			return datePeriod{time.Date(y, m, d, 0, 0, 0, 0, loc), time.Date(y, m, d+1, 0, 0, 0, 0, loc)}, nil
		case "2006-01":
			return datePeriod{time.Date(y, m, 1, 0, 0, 0, 0, loc), time.Date(y, m+1, 1, 0, 0, 0, 0, loc)}, nil
		default:
			return datePeriod{time.Date(y, 1, 1, 0, 0, 0, 0, loc), time.Date(y+1, 1, 1, 0, 0, 0, 0, loc)}, nil
		}
	}
	return datePeriod{}, fmt.Errorf("%q is not YYYY, YYYY-MM or YYYY-MM-DD", s)
}

// parseDateRange reads a dm: value into an inclusive lower and exclusive
// upper bound. A single period selects itself; > starts after it and <
// ends before it.
func parseDateRange(value string) (after, before time.Time, err error) {
	var p datePeriod
	switch {
	case strings.HasPrefix(value, ">="):
		if p, err = parseDate(value[2:]); err == nil {
			after = p.start
		}
	case strings.HasPrefix(value, "<="):
		if p, err = parseDate(value[2:]); err == nil {
			before = p.end
		}
	case strings.HasPrefix(value, ">"):
		if p, err = parseDate(value[1:]); err == nil {
			after = p.end
		}
	case strings.HasPrefix(value, "<"):
		if p, err = parseDate(value[1:]); err == nil {
			before = p.start
		}
	case strings.Contains(value, ".."):
		a, b, _ := strings.Cut(value, "..")
		var q datePeriod
		if p, err = parseDate(a); err != nil {
			return
		}
		if q, err = parseDate(b); err != nil {
			return
		}
		if !p.start.Before(q.end) {
			return after, before, fmt.Errorf("%q is an empty range", value)
		}
		after, before = p.start, q.end
	default:
		if p, err = parseDate(value); err == nil {
			after, before = p.start, p.end
		}
	}
	return after, before, err
}

// anchor picks the term the index lookup is driven by: the longest positive
// literal in rune count, ties to the first seen, among bare words, the
// literal runs of a glob, the components of a path, and ".ext" when exactly
// one positive extension is asked for. short is true when the winner has
// fewer than three runes and must use the short-name postings instead of
// the trigram index. An empty anchor means the whole tree is walked under
// the budget.
func (f Filter) anchor() (anchor string, short bool) {
	anchor, _, _ = f.anchorSource()
	return anchor, anchor != "" && utf8.RuneCountInString(anchor) < 3
}

// anchorSource is anchor with where it came from. fromPath says the anchor
// is a path component, so a match can be a descendant of the anchored node
// and the search must expand subtrees, as the old path query did; prefix is
// then the path term up to and including that component, folded, which the
// path of every seed worth expanding contains.
func (f Filter) anchorSource() (anchor string, fromPath bool, prefix string) {
	consider := func(candidate string, path bool, pathPrefix string) {
		if utf8.RuneCountInString(candidate) > utf8.RuneCountInString(anchor) {
			anchor, fromPath, prefix = candidate, path, pathPrefix
		}
	}
	for _, t := range f.Terms {
		if t.Negate {
			continue
		}
		if !t.Glob {
			consider(t.Text, false, "")
			continue
		}
		for _, run := range strings.FieldsFunc(t.Text, func(r rune) bool { return r == '*' || r == '?' }) {
			consider(run, false, "")
		}
	}
	for _, p := range f.Paths {
		components := strings.Split(p, "/")
		for i, component := range components {
			consider(component, true, strings.Join(components[:i+1], "/"))
		}
	}
	if len(f.Ext) == 1 {
		consider("."+f.Ext[0], false, "")
	}
	return strings.ToLower(anchor), fromPath, strings.ToLower(prefix)
}

// globPattern folds a wildcard term for SQLite GLOB, which is case
// sensitive and treats [ as a class opener. A literal * or ? cannot be
// searched for; that is the grammar's price for wildcards.
func globPattern(text string) string {
	return strings.ReplaceAll(strings.ToLower(text), "[", "[[]")
}

// globLiteral escapes every GLOB metacharacter, for an extension pattern.
func globLiteral(text string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(text) {
		switch r {
		case '*', '?', '[':
			b.WriteString("[" + string(r) + "]")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
