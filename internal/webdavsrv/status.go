package webdavsrv

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
)

// The DAV handler flattens every filesystem failure onto one status per verb:
// any PUT that cannot open its target is 404, any DELETE that cannot remove is
// 405. On a mount configured read-only that tells the user "not found" about a
// file whose parent they can list, which is the kind of answer that sends
// someone looking for a bug that is not there.
//
// The adapter cannot change the status from inside the FileSystem call, so it
// records why a call was refused and corrects the status on the way out. The
// handler still runs unchanged, so lock enforcement and the rest of its
// behaviour are untouched.

type outcomeKey struct{}

type writeOutcome struct{ permissionDenied bool }

func withOutcome(r *http.Request) (*http.Request, *writeOutcome) {
	out := &writeOutcome{}
	return r.WithContext(context.WithValue(r.Context(), outcomeKey{}, out)), out
}

// notePermissionDenied marks the current request as refused by policy rather
// than by absence. It is a no-op outside a write request.
func notePermissionDenied(ctx context.Context, err error) error {
	if errors.Is(err, fs.ErrPermission) {
		if out, ok := ctx.Value(outcomeKey{}).(*writeOutcome); ok {
			out.permissionDenied = true
		}
	}
	return err
}

// correctedWriter rewrites the status the handler chose when the adapter knows
// better, and drops an ETag the handler could not have computed correctly.
type correctedWriter struct {
	http.ResponseWriter
	outcome   *writeOutcome
	dropETag  bool
	wroteHead bool
}

func (w *correctedWriter) WriteHeader(status int) {
	if w.wroteHead {
		return
	}
	w.wroteHead = true
	if w.dropETag {
		// The handler asks the file for its ETag before the write is
		// committed, so the value describes the previous content. Omitting it
		// is allowed; returning a validator that will not match the next GET
		// is not.
		w.Header().Del("ETag")
	}
	if w.outcome.permissionDenied && (status == http.StatusNotFound || status == http.StatusMethodNotAllowed) {
		status = http.StatusForbidden
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *correctedWriter) Write(b []byte) (int, error) {
	if !w.wroteHead {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

// correctWriteStatus wraps the mutating verbs. Read verbs go straight through:
// nothing about them needs correcting, and wrapping them would put an extra
// layer on the byte path.
func correctWriteStatus(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut, http.MethodDelete, "MKCOL", "MOVE", "COPY", "PROPPATCH":
		default:
			next.ServeHTTP(w, r)
			return
		}
		req, outcome := withOutcome(r)
		next.ServeHTTP(&correctedWriter{
			ResponseWriter: w, outcome: outcome, dropETag: r.Method == http.MethodPut,
		}, req)
	})
}

// checkMoveDestination answers a MOVE whose destination is outside this
// namespace before the handler turns it into a 404. A destination on another
// host is a cross-server move (502 by RFC 4918); one on this host but outside
// the exported collection is refused (403). "Not found" would be wrong for
// both: the problem is the destination, not the source.
func checkMoveDestination(next http.Handler, prefix string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "MOVE" {
			next.ServeHTTP(w, r)
			return
		}
		raw := r.Header.Get("Destination")
		if raw == "" || len(raw) > 8192 || strings.ContainsAny(raw, "\x00\r\n") {
			http.Error(w, "Destination header is missing or malformed", http.StatusBadRequest)
			return
		}
		u, err := url.Parse(raw)
		if err != nil {
			http.Error(w, "Destination header is not a valid URI", http.StatusBadRequest)
			return
		}
		if u.Host != "" && !strings.EqualFold(u.Host, r.Host) {
			http.Error(w, "cross-server move is not supported", http.StatusBadGateway)
			return
		}
		if _, ok := stripPrefix(u.Path, prefix); !ok {
			http.Error(w, "destination is outside the DAV namespace", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
