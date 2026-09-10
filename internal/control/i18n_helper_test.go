package control

import (
	"net/http"
	"net/http/httptest"

	"cloudfs/internal/i18n"
)

func newTestRequest(query, header string) *http.Request {
	target := "/doctor"
	if query != "" {
		target += "?lang=" + query
	}
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if header != "" {
		req.Header.Set("Accept-Language", header)
	}
	return req
}

// langRequest is a request that asks for one language, for the helpers that
// take the request rather than the language.
func langRequest(lang i18n.Lang) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/?lang="+string(lang), nil)
	return req
}
