package control

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"

	"cloudfs/internal/index"
)

// CallIndex sends one /index request to the running daemon. method and
// target are the HTTP method and route (query included); body, when not
// nil, is marshalled as the JSON request and out receives the reply. online
// is false when no daemon answers, in which case the CLI opens index.db
// read-only for what it can answer alone.
func CallIndex(ctx context.Context, socket, tcp, method, target string, body, out any) (online bool, err error) {
	var raw []byte
	if body != nil {
		if raw, err = json.Marshal(body); err != nil {
			return false, err
		}
	}
	return callControl(ctx, socket, tcp, method, target, raw, out)
}

// CallIndexStatus asks the running daemon for the index status, of one path
// when p is not empty. Disabled is true when the daemon answered
// {"enabled":false}.
func CallIndexStatus(ctx context.Context, socket, tcp, p string) (st index.Status, online bool, err error) {
	params := url.Values{}
	setIf(params, "path", p)
	target := "/index/status"
	if len(params) > 0 {
		target += "?" + params.Encode()
	}
	online, err = CallIndex(ctx, socket, tcp, http.MethodGet, target, nil, &st)
	return st, online, err
}

// CallIndexSearch runs one content search through the running daemon.
func CallIndexSearch(ctx context.Context, socket, tcp string, q index.SearchQuery) (index.SearchResult, bool, error) {
	params := url.Values{}
	params.Set("q", q.Query)
	if len(q.Roots) > 0 {
		params.Set("path", q.Roots[0])
	}
	setIf(params, "mode", q.Mode)
	if q.TopK > 0 {
		params.Set("limit", strconv.Itoa(q.TopK))
	}
	var out index.SearchResult
	online, err := CallIndex(ctx, socket, tcp, http.MethodGet, "/index/search?"+params.Encode(), nil, &out)
	return out, online, err
}
