package control

import (
	"context"
	"encoding/json"
	"net/http"
)

// PoolRequest is what `cloudfs pool` sends.
type PoolRequest struct {
	Action string `json:"-"`
	Body   any    `json:"-"`
}

// CallPool performs one pool action against the running daemon. status and
// divergences are GETs; everything else is a POST with the control header.
func CallPool(ctx context.Context, socket, tcp, action string, body any, out any) (bool, error) {
	method, route := http.MethodPost, "/pool/"+action
	var raw []byte
	switch action {
	case "status", "divergences":
		method = http.MethodGet
	default:
		b, err := json.Marshal(body)
		if err != nil {
			return false, err
		}
		raw = b
	}
	return callControl(ctx, socket, tcp, method, route, raw, out)
}
