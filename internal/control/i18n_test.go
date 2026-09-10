package control

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"cloudfs/internal/i18n"
	"cloudfs/internal/provider"
)

// TestCheckLocalizeRerendersDetailAndFix: a check is built in English so a
// caller that never negotiates a language still reads a sentence; asking for
// another language re-renders it from the same key and arguments.
func TestCheckLocalizeRerendersDetailAndFix(t *testing.T) {
	c := Check{Name: "cache_dir", Level: LevelOK}
	c.setDetail("doctor.cache.writable", "/var/cache")
	c.setFix("doctor.cache.fix.perms")

	if c.Detail != "/var/cache is writable" {
		t.Fatalf("english detail = %q", c.Detail)
	}
	zh := c.Localize(i18n.ZH)
	if zh.Detail != "/var/cache 可写" {
		t.Fatalf("chinese detail = %q", zh.Detail)
	}
	if !strings.Contains(zh.Fix, "缓存目录") {
		t.Fatalf("chinese fix = %q", zh.Fix)
	}
	if c.Detail != "/var/cache is writable" {
		t.Fatalf("Localize mutated the receiver: %q", c.Detail)
	}
}

// TestLocalizeLeavesUnkeyedTextAlone: details taken from an error string have
// no catalog entry and must survive translation untouched.
func TestLocalizeLeavesUnkeyedTextAlone(t *testing.T) {
	c := Check{Name: "metadata_db", Level: LevelFail, Detail: "disk I/O error"}
	if got := c.Localize(i18n.ZH).Detail; got != "disk I/O error" {
		t.Fatalf("detail = %q, want it unchanged", got)
	}
}

// TestLangFromRequestPrefersTheExplicitQuery: the UI's language switch must
// win over the browser header, because the person just chose it.
func TestLangFromRequestPrefersTheExplicitQuery(t *testing.T) {
	for _, tc := range []struct {
		query, header string
		want          i18n.Lang
	}{
		{"", "en-US,en;q=0.9", i18n.EN},
		{"en", "zh-CN", i18n.EN},
		{"zh", "en-US", i18n.ZH},
		{"fr", "en-US", i18n.EN},
		{"", "", i18n.ZH},
	} {
		req := newTestRequest(tc.query, tc.header)
		if got := LangFrom(req); got != tc.want {
			t.Fatalf("lang(query=%q, header=%q) = %q, want %q", tc.query, tc.header, got, tc.want)
		}
	}
}

// TestStatusWarningsFollowTheRequestLanguage: the titlebar reads these, so a
// page asking for English must not get a Chinese warning beside its English
// diagnostics.
func TestStatusWarningsFollowTheRequestLanguage(t *testing.T) {
	render := func(lang i18n.Lang) []string {
		var s Status
		s.Uploads.Dead = 2
		warnings(&s, lang)
		return s.Warnings
	}
	en, zh := render(i18n.EN), render(i18n.ZH)
	if len(en) != 1 || !strings.Contains(en[0], "failed permanently") {
		t.Fatalf("english warnings = %v", en)
	}
	if len(zh) != 1 || !strings.Contains(zh[0], "永久失败") {
		t.Fatalf("chinese warnings = %v", zh)
	}
}

// TestDoctorEndpointAnswersInTheRequestedLanguage: the diagnostics page is the
// one screen a person reads when something is already wrong, so its findings
// must arrive in the language the page asked for.
func TestDoctorEndpointAnswersInTheRequestedLanguage(t *testing.T) {
	f := newFixture(t)
	f.coll.Doctor = &Doctor{CacheDir: filepath.Join(f.dir, "cache")}
	srv := NewServer(f.coll)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	get := func(header string) []Check {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/doctor/run", nil)
		if err != nil {
			t.Fatal(err)
		}
		if header != "" {
			req.Header.Set("Accept-Language", header)
		}
		req.Header.Set("X-CloudFS-Control", "1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("doctor/run = %d", resp.StatusCode)
		}
		var out DoctorResponse
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out.Checks
	}
	detail := func(checks []Check, name string) string {
		for _, c := range checks {
			if c.Name == name {
				return c.Detail
			}
		}
		t.Fatalf("no check named %q in %v", name, checks)
		return ""
	}
	if got := detail(get("en-US,en;q=0.9"), "cache_dir"); !strings.HasSuffix(got, "is writable") {
		t.Fatalf("english cache_dir detail = %q", got)
	}
	if got := detail(get("zh-CN,zh;q=0.9"), "cache_dir"); !strings.HasSuffix(got, "可写") {
		t.Fatalf("chinese cache_dir detail = %q", got)
	}
}

// TestAccountPromptsFollowTheRequestLanguage: a driver registers one prompt in
// one language; the catalog is what makes the add-drive form readable in the
// other.
func TestAccountPromptsFollowTheRequestLanguage(t *testing.T) {
	provider.RegisterFields("quark", []provider.Field{
		{Name: "root_id", Prompt: "作为根的目录 id", Default: "0"},
	}, provider.Credentials{Fields: []string{"cookie"}, Note: "浏览器登录后的 cookie"})

	f := newFixture(t)
	srv := NewServer(f.coll)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	get := func(query string) AccountsResponse {
		t.Helper()
		resp, err := http.Get(ts.URL + "/accounts" + query)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out AccountsResponse
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	promptOf := func(out AccountsResponse) (string, string) {
		for _, tp := range out.Types {
			if tp.Type != "quark" {
				continue
			}
			for _, fl := range tp.Fields {
				if fl.Name == "root_id" {
					return fl.Prompt, tp.Credentials
				}
			}
		}
		t.Fatal("quark/root_id missing from /accounts")
		return "", ""
	}
	prompt, creds := promptOf(get("?lang=en"))
	if prompt != "Directory id to use as the root" {
		t.Fatalf("english prompt = %q", prompt)
	}
	if !strings.Contains(creds, "unofficial API") {
		t.Fatalf("english credential note = %q", creds)
	}
	if prompt, _ := promptOf(get("?lang=zh")); prompt != "作为根的目录 id" {
		t.Fatalf("chinese prompt = %q", prompt)
	}
}

// TestControlErrorsAreLocalized: the page shows an error response verbatim in
// a toast, so a refusal must arrive in the language the page asked for.
func TestControlErrorsAreLocalized(t *testing.T) {
	f := newFixture(t)
	ts := httptest.NewServer(NewServer(f.coll).Handler())
	defer ts.Close()

	body := func(lang string) string {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, ts.URL+"/doctor/run?lang="+lang, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-CloudFS-Control", "1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405", resp.StatusCode)
		}
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(b))
	}
	if got := body("en"); got != "method not allowed" {
		t.Fatalf("english error = %q", got)
	}
	if got := body("zh"); got != "不支持这个方法" {
		t.Fatalf("chinese error = %q", got)
	}
}

func TestListLimitErrorsAreLocalized(t *testing.T) {
	f, _ := fsControl(t)
	ts := httptest.NewServer(NewServer(f.coll).Handler())
	defer ts.Close()

	for _, tc := range []struct {
		lang string
		want string
	}{
		{lang: "en", want: "limit must be 1..1000"},
		{lang: "zh", want: "limit 必须在 1..1000 范围内"},
	} {
		resp, err := http.Get(ts.URL + "/fs/list?path=/&limit=bad&lang=" + tc.lang)
		if err != nil {
			t.Fatal(err)
		}
		b, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			t.Fatal(readErr)
		}
		if resp.StatusCode != http.StatusBadRequest || strings.TrimSpace(string(b)) != tc.want {
			t.Fatalf("lang=%s: status=%d body=%q", tc.lang, resp.StatusCode, strings.TrimSpace(string(b)))
		}
	}
}

// TestConfirmationPromptsAreLocalized: the sentence naming what is about to
// happen is the last thing between a person and a destructive call, so it is
// the last place to leave in a language they do not read.
func TestConfirmationPromptsAreLocalized(t *testing.T) {
	refuse := func(lang i18n.Lang) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		confirmed(rr, langRequest(lang), false, "confirm.delete_path", "/photos/a.jpg")
		return rr
	}
	en, zh := refuse(i18n.EN), refuse(i18n.ZH)
	if en.Code != http.StatusBadRequest || !strings.Contains(en.Body.String(), "deletes /photos/a.jpg on the remote") {
		t.Fatalf("english prompt = %d %s", en.Code, en.Body)
	}
	if zh.Code != http.StatusBadRequest || !strings.Contains(zh.Body.String(), "连远端上的 /photos/a.jpg 一起删除") {
		t.Fatalf("chinese prompt = %d %s", zh.Code, zh.Body)
	}
	ok := httptest.NewRecorder()
	if !confirmed(ok, langRequest(i18n.ZH), true, "confirm.delete_path", "x") || ok.Code != http.StatusOK {
		t.Fatal("a confirmed call must not be refused")
	}
}
