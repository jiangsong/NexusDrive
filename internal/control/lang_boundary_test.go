package control

import (
	"net/http"
	"strings"
	"testing"
)

// The language is resolved once, at the edge, and taken out of the query
// before any handler sees it. Subtracting it inside each strict handler was
// the same shape as a route forgetting privateRequest: correct only for the
// handlers somebody remembered, and the next one would answer 400 on load
// with the symptom (a blank screen) far from the cause. This walks every
// route the server declares.
func TestEveryRouteToleratesTheLanguageParameter(t *testing.T) {
	srv, _, _ := accountsServer(t)
	for _, rt := range srv.routes() {
		target := rt.pattern
		if rt.probe != "" {
			target = rt.probe
		}
		if strings.HasSuffix(target, "/") {
			continue // a prefix with no probe is not a route anyone calls
		}
		if target == "/events" {
			continue // an SSE stream never returns; its language is covered by events_test.go
		}
		plain := accountRequest(t, srv, http.MethodGet, target, nil)
		withLang := accountRequest(t, srv, http.MethodGet, target+"?lang=en", nil)
		if plain.Code != withLang.Code {
			t.Errorf("%s answers %d without the language and %d with it: %s",
				target, plain.Code, withLang.Code, strings.TrimSpace(withLang.Body.String()))
		}
	}
}

// Resolving at the edge must not make the parameter invisible to the choice
// it encodes: an explicit language still beats the browser's header.
func TestTheResolvedLanguageStillPrefersTheExplicitChoice(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "/doctor/run?lang=en", nil)
	req.Host = "127.0.0.1:9101"
	req.Header.Set("Accept-Language", "zh")
	got := LangFrom(resolveLang(req))
	if string(got) != "en" {
		t.Fatalf("the explicit choice lost to the header: %q", got)
	}
	if v := resolveLang(req).URL.Query().Get("lang"); v != "" {
		t.Fatalf("the parameter survived into the handler: %q", v)
	}
}
