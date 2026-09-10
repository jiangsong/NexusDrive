package i18n

import "testing"

// TestMatchPrefersTheClientsFirstSupportedLanguage: the header is a ranked
// list, so the winner is the highest-quality entry this build can render, not
// simply the first token.
func TestMatchPrefersTheClientsFirstSupportedLanguage(t *testing.T) {
	for _, tc := range []struct {
		header string
		want   Lang
	}{
		{"", ZH},
		{"zh-CN,zh;q=0.9", ZH},
		{"en-US,en;q=0.9", EN},
		{"fr-FR,fr;q=0.9", ZH},
		{"fr;q=1.0, en;q=0.8, zh;q=0.5", EN},
		{"zh-Hans-CN", ZH},
		{"EN-gb", EN},
		{"*", ZH},
	} {
		if got := Match(tc.header); got != tc.want {
			t.Fatalf("Match(%q) = %q, want %q", tc.header, got, tc.want)
		}
	}
}

// TestTFallsBackToChineseThenToTheKey: an untranslated key must still render
// something a person can act on, and must never render empty.
func TestTFallsBackToChineseThenToTheKey(t *testing.T) {
	if got := T(EN, "doctor.queue.idle"); got != "no queued uploads" {
		t.Fatalf("english lookup = %q", got)
	}
	if got := T(ZH, "doctor.queue.idle"); got != "没有排队的上传" {
		t.Fatalf("chinese lookup = %q", got)
	}
	if got := T(EN, "no.such.key.anywhere"); got != "no.such.key.anywhere" {
		t.Fatalf("missing key = %q, want the key itself", got)
	}
}

// TestTFormatsArguments: doctor details carry counts and paths, so the
// catalog entries are format strings.
func TestTFormatsArguments(t *testing.T) {
	if got := T(EN, "doctor.cache.writable", "/var/cache"); got != "/var/cache is writable" {
		t.Fatalf("formatted = %q", got)
	}
	if got := T(ZH, "doctor.cache.writable", "/var/cache"); got != "/var/cache 可写" {
		t.Fatalf("formatted = %q", got)
	}
}

// TestEveryEnglishKeyHasAChineseTwin: the two tables are edited by hand, so a
// key added to one and forgotten in the other is the expected failure mode.
func TestEveryEnglishKeyHasAChineseTwin(t *testing.T) {
	for k := range en {
		if _, ok := zh[k]; !ok {
			t.Errorf("key %q is in en but not zh", k)
		}
	}
	for k := range zh {
		if _, ok := en[k]; !ok {
			t.Errorf("key %q is in zh but not en", k)
		}
	}
}
