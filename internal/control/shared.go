package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"cloudfs/internal/config"
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

// errConfirmRequired is the answer to a destructive request that did not say
// confirm. The message names what would have happened, so a caller reading it
// knows what they are being asked to confirm.
var errConfirmRequired = errors.New("control: confirm is required")

// requireConfirm gates an action that cannot be undone. The uploads resume and
// drop actions established the convention: the request carries confirm=true,
// and the UI only sets it after the person typed the identifier of the thing
// they are about to lose.
func requireConfirm(confirm bool, consequence string) error {
	if confirm {
		return nil
	}
	return fmt.Errorf("%w: this %s; send confirm=true after checking the target", errConfirmRequired, consequence)
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
