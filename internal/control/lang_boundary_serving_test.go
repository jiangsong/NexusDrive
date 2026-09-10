package control

import (
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
)

// The language boundary has to sit on the handler the daemon actually serves,
// not only on the one tests reach through Handler(). Start and ListenAndServe
// built their own chain, so ?lang= survived into handlers that reject any
// query string and the page 400ed on load. This drives a real listener.
func TestServedHandlerStripsTheLanguageParameter(t *testing.T) {
	f := newFixture(t)
	p := socketPath(t)
	r, err := NewServer(f.coll).Start(context.Background(), p, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })

	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", p)
		},
	}}
	get := func(target string) int {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, "http://cloudfs"+target, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	for _, target := range []string{"/copies?limit=100", "/uploads", "/status"} {
		separator := "?"
		if strings.Contains(target, "?") {
			separator = "&"
		}
		plain := get(target)
		withLang := get(target + separator + "lang=zh")
		if plain != withLang {
			t.Errorf("%s answers %d without the language and %d with it", target, plain, withLang)
		}
	}
}
