package control

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"cloudfs/internal/config"
	"cloudfs/internal/i18n"
)

// writeJSON answers with no-store. Every reply on this server names something
// about this machine — configured remotes, paths, queue contents — and none of
// it belongs in a cache.
func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_ = json.NewEncoder(w).Encode(body)
}

// confirmed gates an action that cannot be undone. The uploads resume and
// drop actions established the convention: the request carries confirm=true,
// and the UI only sets it after the person typed the identifier of the thing
// they are about to lose. It answers the request itself when the flag is
// missing, naming the consequence in the reader's language, so a caller is
// one line rather than four.
func confirmed(w http.ResponseWriter, r *http.Request, confirm bool, consequenceKey string, args ...any) bool {
	if confirm {
		return true
	}
	lang := LangFrom(r)
	http.Error(w, i18n.T(lang, "confirm.required", i18n.T(lang, consequenceKey, args...)), http.StatusBadRequest)
	return false
}

// rejectSecretFields is the credential boundary of the control plane, in one
// place. A key that config.IsSecretField recognises is refused before anything
// is written or logged: a credential never arrives over this API, so it can
// never be proxied, cached or left in a browser's memory by it. Both the
// account-creation and account-editing endpoints call this; the check must not
// exist twice.
func rejectSecretFields(remote string, fields map[string]string) error {
	for key := range fields {
		if config.IsSecretField(key) {
			return fmt.Errorf("%s is a credential; set it with `cloudfs config auth %s`", key, remote)
		}
	}
	return nil
}

// allowMethod answers the request itself when the method is wrong, naming
// what is allowed. Twenty-odd routes were each spelling out the Allow header
// and the 405 by hand; the pair belongs together, so it is one call.
func allowMethod(w http.ResponseWriter, r *http.Request, methods ...string) bool {
	for _, m := range methods {
		if r.Method == m {
			return true
		}
	}
	w.Header().Set("Allow", strings.Join(methods, ", "))
	httpErrorT(w, r, http.StatusMethodNotAllowed, "err.method_not_allowed")
	return false
}
