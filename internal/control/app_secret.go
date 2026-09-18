package control

import (
	"errors"
	"net/http"
	"strings"

	"cloudfs/internal/config"
)

// The OAuth application secret is the single exception to "no credential
// travels through this API", and it is an exception because the rule was
// otherwise unsatisfiable: a backend whose application the person registers
// themselves — Google Drive, whose restricted scope means no application ships
// with the binary — cannot begin any authorization until its secret is on
// disk. Refusing it here did not keep the value out of the browser; it sent
// the person to a terminal to type it there instead, which is the editing-YAML
// over-SSH problem this whole surface exists to remove.
//
// What keeps the exception narrow:
//
//   - Only the application secret. The request carries one field, and the
//     decoder refuses a body with any other, so the account's own token can
//     never enter by this door.
//   - Only where it is used. A backend with no confidential OAuth client is
//     refused before the file is touched, so the route cannot become a general
//     credential importer by accident.
//   - Stored the way every other secret is: the keyring, or a 0600 file, never
//     the configuration.
//   - It does not mark the account authorized. The sign-in still has to happen,
//     and it still happens in the daemon.

// AppSecretRequest is POST /accounts/{name}/auth/app-secret.
type AppSecretRequest struct {
	ClientSecret string `json:"client_secret"`
}

// appSecretMax bounds what is accepted. Real ones are tens of characters; this
// is wide enough for any provider and narrow enough that the route cannot be
// used to push a file into the secret store.
const appSecretMax = 4096

func (s *Server) authAppSecret(w http.ResponseWriter, r *http.Request, name string) {
	cfg := s.collector.ConfigView()
	if cfg == nil || cfg.SourcePath == "" {
		httpErrorT(w, r, http.StatusConflict, "err.no_config")
		return
	}
	remote, ok := cfg.Remotes[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	if s.auth == nil || s.auth.AppSecret == nil || !s.auth.AppSecret(remote.Type) {
		httpErrorT(w, r, http.StatusBadRequest, "err.app_secret_not_used", remote.Type)
		return
	}
	var in AppSecretRequest
	if !decodeMutationLimit(w, r, &in, 8<<10) {
		return
	}
	secret := strings.TrimSpace(in.ClientSecret)
	if secret == "" || len(secret) > appSecretMax || strings.ContainsAny(secret, "\x00\r\n") {
		httpErrorT(w, r, http.StatusBadRequest, "err.app_secret_invalid")
		return
	}
	// The binding is preserved on purpose. It fences queued uploads to one
	// account identity, and storing the registration's secret does not change
	// which account this is — the sign-in that follows rotates it, as every
	// new authorization does.
	if _, err := config.SaveCredentialsForRemotePreservingBinding(cfg.SourcePath, name, map[string]string{"client_secret": secret}, remote); err != nil {
		if errors.Is(err, config.ErrCredentialsChanged) {
			httpErrorT(w, r, http.StatusConflict, "err.account_changed")
			return
		}
		// Never the error text: it can quote the value it failed to store.
		httpErrorT(w, r, http.StatusInternalServerError, "err.app_secret_store_failed")
		return
	}
	s.reloadConfigView()
	d, _ := s.accountDetail(name)
	writeJSON(w, AccountMutationResponse{Name: name, RestartRequired: true, Detail: &d})
}
