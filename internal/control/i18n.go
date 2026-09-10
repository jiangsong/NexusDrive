package control

import (
	"context"
	"net/http"
	"strings"

	"cloudfs/internal/i18n"
)

// langKey addresses the resolved language in a request's context.
type langKey struct{}

// resolveLang decides the language once, at the edge, and takes the parameter
// out of the query. Every call the page makes carries ?lang=, because
// EventSource cannot set a header, and a handler that refuses unknown query
// parameters would otherwise refuse every call from the browser. Subtracting
// it inside each such handler is the same shape as a route forgetting
// privateRequest — right for the handlers somebody remembered, and a 400 on
// load for the next one. Here it is invisible to all of them at once.
func resolveLang(r *http.Request) *http.Request {
	lang := langOf(r)
	q := r.URL.Query()
	if _, ok := q["lang"]; ok {
		q.Del("lang")
		next := *r.URL
		next.RawQuery = q.Encode()
		r = r.Clone(context.WithValue(r.Context(), langKey{}, lang))
		r.URL = &next
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), langKey{}, lang))
}

// langOf reads the language a request asks for. An explicit ?lang= wins over
// the browser's Accept-Language: it is the UI's language switch, and a person
// who just chose a language should not be overruled by the header their
// browser happens to send. An unsupported value falls back to the header
// rather than to the default, so a stale bookmark does not pin the page to
// Chinese for an English reader.
func langOf(r *http.Request) i18n.Lang {
	if v := r.URL.Query().Get("lang"); i18n.Valid(v) {
		return i18n.Lang(v)
	}
	return i18n.Match(r.Header.Get("Accept-Language"))
}

// LangFrom is what handlers call. The edge has already decided; the fallback
// covers a handler exercised directly by a test, without a server around it.
func LangFrom(r *http.Request) i18n.Lang {
	if r == nil {
		return i18n.ZH
	}
	if lang, ok := r.Context().Value(langKey{}).(i18n.Lang); ok {
		return lang
	}
	return langOf(r)
}

// message is a sentence not yet in any language: a catalog key and the
// arguments it is rendered with. An empty key marks text that came from an
// error or a remote, which has no translation and is passed through.
type message struct {
	key  string
	args []any
	// raw marks a fragment that is not a catalog entry at all — an error
	// string, a member state, a remote's own words. It renders as itself in
	// every language instead of being looked up, so a key-shaped value
	// cannot be translated into something the source never said.
	raw bool
}

// render joins a list of fragments in one language.
func render(lang i18n.Lang, msgs []message) string {
	parts := make([]string, 0, len(msgs))
	for _, m := range msgs {
		if m.raw {
			parts = append(parts, m.key)
			continue
		}
		parts = append(parts, i18n.T(lang, m.key, m.args...))
	}
	return strings.Join(parts, "; ")
}

// setDetail records what was observed, as a catalog key plus its arguments,
// and renders the English form now. Storing the key is what lets the HTTP
// layer answer the same check in another language without the doctor knowing
// a language exists.
func (c *Check) setDetail(key string, args ...any) {
	c.detail = []message{{key: key, args: args}}
	c.Detail = render(i18n.EN, c.detail)
}

// addDetail adds a clause to what the check already said. Appending to the
// rendered string instead would put the clause outside the key, where
// Localize — which rebuilds Detail from the keys — would drop it.
func (c *Check) addDetail(key string, args ...any) {
	c.detail = append(c.detail, message{key: key, args: args})
	c.Detail = render(i18n.EN, c.detail)
}

// passDetail records text that has no translation — an error, a member state,
// a remote's own message. Assigning it to Detail directly instead would leave
// it outside c.detail, where the next addDetail rebuilds Detail from the
// fragments and drops it.
func (c *Check) passDetail(text string) {
	c.detail = []message{{key: text, raw: true}}
	c.Detail = render(i18n.EN, c.detail)
}

// setFix records the advice, with the same contract as setDetail.
func (c *Check) setFix(key string, args ...any) {
	c.fix = []message{{key: key, args: args}}
	c.Fix = render(i18n.EN, c.fix)
}

// Localize returns a copy of the check rendered in lang. Text with no key —
// an error string, a member state, a proxy's own failure message — is passed
// through: it was never a catalog entry and must not be replaced by one.
func (c Check) Localize(lang i18n.Lang) Check {
	if len(c.detail) > 0 {
		c.Detail = render(lang, c.detail)
	}
	if len(c.fix) > 0 {
		c.Fix = render(lang, c.fix)
	}
	return c
}

// LocalizeChecks renders a whole report in lang.
func LocalizeChecks(checks []Check, lang i18n.Lang) []Check {
	out := make([]Check, len(checks))
	for i, c := range checks {
		out[i] = c.Localize(lang)
	}
	return out
}

// httpErrorT writes a control API error in the request's language. The page
// shows the response body verbatim in a toast, so these sentences are read by
// a person, not only by a client. Errors that came from the operating system,
// a remote or a driver keep their own text: they have no catalog entry, and
// inventing one would hide the words a bug report needs.
func httpErrorT(w http.ResponseWriter, r *http.Request, status int, key string, args ...any) {
	http.Error(w, i18n.T(LangFrom(r), key, args...), status)
}
