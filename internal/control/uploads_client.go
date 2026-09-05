package control

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
)

// CallUploads never replays a request after connecting. The caller's context
// controls long-running flush operations; there is no implicit HTTP timeout.
func CallUploads(ctx context.Context, socket, tcp string, q UploadRequest) (UploadResponse, bool, error) {
	var out UploadResponse
	if err := q.Validate(); err != nil {
		return out, false, err
	}
	method, route := http.MethodPost, "/uploads/"+q.Action
	body, err := json.Marshal(q)
	if err != nil {
		return out, false, err
	}
	if q.Action == "list" {
		method = http.MethodGet
		params := url.Values{"cursor": {q.Cursor}}
		if q.Limit != 0 {
			params.Set("limit", strconv.Itoa(q.Limit))
		}
		route = "/uploads?" + params.Encode()
		body = nil
	}
	online, err := callControl(ctx, socket, tcp, method, route, body, &out)
	return out, online, err
}
