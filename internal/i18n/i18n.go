// Package i18n holds the message catalog for every string this daemon shows
// to a person: doctor findings, the add-drive prompts, wizard questions and
// the desktop shell's placeholders.
//
// The catalog exists because the same sentence is rendered from three places
// — the web UI, the CLI wizard and the desktop window — and a table keyed by
// language in each of them goes stale the moment one of the three is edited.
// Callers hold a Lang (negotiated once, from an Accept-Language header or the
// CLOUDFS_LANG environment variable) and pass it down; nothing below the
// control layer knows about locales.
package i18n

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Lang is a supported message language. It is deliberately not a full BCP 47
// tag: the catalog is hand written, so the set is closed.
type Lang string

const (
	// ZH is Simplified Chinese, the default and the fallback: it is the only
	// language every key is guaranteed to have.
	ZH Lang = "zh"
	// EN is English.
	EN Lang = "en"
)

// Supported lists the languages a front end may offer, most preferred first.
var Supported = []Lang{ZH, EN}

// Valid reports whether s names a language this build can render.
func Valid(s string) bool {
	for _, l := range Supported {
		if string(l) == s {
			return true
		}
	}
	return false
}

// Match negotiates a language from an Accept-Language header value. An empty,
// malformed or entirely unsupported header yields ZH.
//
// The header is a ranked list, so the answer is the supported entry with the
// highest quality, not the first token: a browser sending "fr, en;q=0.8,
// zh;q=0.5" wants English out of what is on offer here.
func Match(header string) Lang {
	// One pass keeping the best (q, position). A header is typically one to
	// three entries, so sorting a slice of structs to read one maximum was
	// more machinery than the question needs.
	best, bestQ, found := ZH, 0.0, false
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		tag, q := part, 1.0
		if semi := strings.Index(part, ";"); semi >= 0 {
			tag = strings.TrimSpace(part[:semi])
			for _, param := range strings.Split(part[semi+1:], ";") {
				param = strings.TrimSpace(param)
				if v, ok := strings.CutPrefix(param, "q="); ok {
					if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
						q = f
					}
				}
			}
		}
		lang, ok := base(tag)
		if !ok || q <= 0 {
			continue
		}
		// Ties go to the client's own order, so the first entry at the
		// highest q wins and a later one does not displace it.
		if !found || q > bestQ {
			best, bestQ, found = lang, q, true
		}
	}
	return best
}

// base maps one language tag to a supported language. Only the primary
// subtag matters: zh-Hans-CN and zh-TW alike render the Chinese table,
// because a separate Traditional catalog does not exist yet.
func base(tag string) (Lang, bool) {
	primary, _, _ := strings.Cut(strings.ToLower(tag), "-")
	switch primary {
	case "zh":
		return ZH, true
	case "en":
		return EN, true
	}
	return "", false
}

// FromEnv reads CLOUDFS_LANG, then the usual POSIX locale variables, so a CLI
// run answers in the shell's language without a flag. It returns ZH when
// nothing names a language this build has.
func FromEnv() Lang {
	if v := strings.TrimSpace(os.Getenv("CLOUDFS_LANG")); v != "" {
		if l, ok := base(v); ok {
			return l
		}
	}
	for _, key := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		v := strings.TrimSpace(os.Getenv(key))
		if v == "" {
			continue
		}
		// en_US.UTF-8 — the encoding and the territory are both noise here.
		v, _, _ = strings.Cut(v, ".")
		v = strings.ReplaceAll(v, "_", "-")
		if l, ok := base(v); ok {
			return l
		}
	}
	return ZH
}

// T renders one catalog entry. A key missing from the requested language
// falls back to Chinese, and a key missing everywhere renders as itself: a
// front end showing "doctor.cache.writable" is wrong but still diagnosable,
// while an empty label is neither.
func T(lang Lang, key string, args ...any) string {
	format, ok := lookupOK(lang, key)
	if !ok {
		// A missing key renders as the key itself: a visible, greppable gap
		// rather than a blank line. Formatting it with the arguments would
		// bury that under "%!(EXTRA string=...)", which is neither the key
		// nor a sentence.
		return key
	}
	if len(args) == 0 {
		return format
	}
	return fmt.Sprintf(format, args...)
}

// lookupOK reports whether the key exists anywhere, so T can tell a real
// translation from the key-as-fallback.
func lookupOK(lang Lang, key string) (string, bool) {
	s := lookup(lang, key)
	return s, s != key
}

func lookup(lang Lang, key string) string {
	if lang == EN {
		if s, ok := en[key]; ok {
			return s
		}
	}
	if s, ok := zh[key]; ok {
		return s
	}
	if s, ok := en[key]; ok {
		return s
	}
	return key
}

// Has reports whether key exists in any catalog. Callers that fall back to a
// literal written at the call site use it to tell "no translation" from "the
// translation happens to equal the key".
func Has(key string) bool {
	if _, ok := zh[key]; ok {
		return true
	}
	_, ok := en[key]
	return ok
}
