package control

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"cloudfs/internal/config"
	"cloudfs/internal/i18n"
	"cloudfs/internal/provider"
)

// Adding a drive meant editing YAML over SSH, which is the first wall a NAS
// user hits. This endpoint lets the status page put the account in the
// configuration instead.
//
// It deliberately stops there. A credential does not travel through this API:
// the page can create the account and is then told the exact `cloudfs config
// auth` command to run. Accepting a cloud drive's password or refresh token
// through a browser form on a machine's loopback interface would put the most
// sensitive value in the system into the one place with the widest attack
// surface, to save one command — and the authorization flows that matter
// (browser OAuth, a phone scanning a QR code) are driven from the terminal
// anyway.

// AccountField describes one public setting a backend needs, mirroring
// provider.Field for the wire.
type AccountField struct {
	Name     string `json:"name"`
	Prompt   string `json:"prompt"`
	Required bool   `json:"required,omitempty"`
	Default  string `json:"default,omitempty"`
	Example  string `json:"example,omitempty"`
}

// AccountType is one backend the page can offer.
type AccountType struct {
	Type   string         `json:"type"`
	Fields []AccountField `json:"fields"`
	// BrowserAuth says the daemon can obtain this backend's credential itself,
	// so the page can offer a button instead of a command to copy. The answer
	// comes from the daemon rather than from a list kept in the page: that
	// list is exactly what went stale when gdrive, box and dropbox were added.
	BrowserAuth bool `json:"browser_auth,omitempty"`
	// Credentials is what `config auth` will ask for, in plain words.
	Credentials string `json:"credentials,omitempty"`
	// AppSecret says the backend authorizes as a confidential OAuth client,
	// so its own registration's secret is part of adding the account.
	AppSecret bool `json:"app_secret,omitempty"`
	// Setup is the walkthrough for a backend whose OAuth application the
	// person has to register themselves, in order. Empty for every backend
	// the daemon can authorize with a shipped registration.
	Setup []string `json:"setup,omitempty"`
}

// AccountSummary is a configured remote, without any credential value.
type AccountSummary struct {
	Name string `json:"name"`
	Type string `json:"type"`
	// HasCredentials says a credential is already stored for this account, so
	// a setup flow interrupted halfway can tell which drives it still has to
	// authorize without fetching each one in turn.
	HasCredentials bool `json:"has_credentials,omitempty"`
}

// AccountsResponse is what GET /accounts returns.
type AccountsResponse struct {
	// Configurable is false when this daemon was started without a config
	// file on disk, so there is nothing to add a remote to.
	Configurable bool             `json:"configurable"`
	ConfigPath   string           `json:"config_path,omitempty"`
	Types        []AccountType    `json:"types"`
	Remotes      []AccountSummary `json:"remotes"`
}

// AddAccountRequest is the body of POST /accounts.
type AddAccountRequest struct {
	Name   string            `json:"name"`
	Type   string            `json:"type"`
	Fields map[string]string `json:"fields"`
	// Mount, when set, also adds a mount entry for the new remote.
	Mount  string `json:"mount,omitempty"`
	Prefix string `json:"prefix,omitempty"`
	Mode   string `json:"mode,omitempty"`
	// Pool, when set, also adds the new remote as a member of that pool:
	// the drive joins the fused space instead of (or as well as) getting
	// a prefix of its own.
	Pool string `json:"pool,omitempty"`
}

// AddAccountResponse tells the caller what to do next.
type AddAccountResponse struct {
	// JoinedPool names the pool the remote was made a member of, if any.
	JoinedPool string `json:"joined_pool,omitempty"`
	Name       string `json:"name"`
	Type       string `json:"type"`
	// NextCommand is the credential step, which this API does not perform.
	NextCommand string `json:"next_command"`
	Credentials string `json:"credentials,omitempty"`
	// RestartRequired: the file changed; the running daemon has not.
	RestartRequired bool `json:"restart_required"`
}

// accountTypes describes every backend the page can offer, with its prompts
// rendered in lang. A driver registers one prompt in whatever language its
// author wrote; the catalog supplies the rest, and a driver with no catalog
// entry still asks its own readable question.
func (c *Collector) accountTypes(lang i18n.Lang) []AccountType {
	types := provider.DescribedTypes()
	out := make([]AccountType, 0, len(types))
	for _, typ := range types {
		entry := AccountType{Type: typ, Credentials: credentialSummary(lang, typ), Setup: setupSteps(lang, typ)}
		for _, f := range provider.Fields(typ) {
			entry.Fields = append(entry.Fields, AccountField{
				Name: f.Name, Prompt: i18n.FieldPrompt(lang, typ, f.Name, f.Prompt), Required: f.Required,
				Default: f.Default, Example: f.Example,
			})
		}
		out = append(out, entry)
	}
	return out
}

// hasStoredCredential reports whether anything secret has been saved for an
// account. It reads only whether a value is present, never the value: an empty
// field means the account was created but never authorized, which is the one
// thing a resumed setup needs to know.
func hasStoredCredential(r config.Remote) bool {
	for k, v := range r.Extra {
		// The application's own secret is not an authorization: a person can
		// register an application and never sign in, and a setup that treated
		// that as done would skip the only step still missing.
		if !config.IsSecretField(k) || config.IsOAuthAppField(k) {
			continue
		}
		if text, ok := v.(string); ok && text != "" {
			return true
		}
	}
	return false
}

// setupSteps renders a backend's registration walkthrough in lang, falling
// back to what the driver wrote when the catalog has no translation — the
// same rule the prompts and the credential note follow.
func setupSteps(lang i18n.Lang, typ string) []string {
	return i18n.CredentialSetup(lang, typ, provider.CredentialsFor(typ).Setup)
}

func credentialSummary(lang i18n.Lang, typ string) string {
	creds := provider.CredentialsFor(typ)
	if note := i18n.CredentialNote(lang, typ, creds.Note); note != "" {
		return note
	}
	return strings.Join(creds.Fields, " or ")
}

func (s *Server) accounts(w http.ResponseWriter, r *http.Request) {
	if !privateRequest(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.listAccounts(w, r)
	case http.MethodPost:
		s.addAccount(w, r)
	default:
		allowMethod(w, r, http.MethodGet, http.MethodPost)
	}
}

func (s *Server) listAccounts(w http.ResponseWriter, r *http.Request) {
	out := AccountsResponse{Types: s.collector.accountTypes(LangFrom(r))}
	if s.auth != nil {
		for i := range out.Types {
			if s.auth.Supported != nil {
				out.Types[i].BrowserAuth = s.auth.Supported(out.Types[i].Type)
			}
			if s.auth.AppSecret != nil {
				out.Types[i].AppSecret = s.auth.AppSecret(out.Types[i].Type)
			}
		}
	}
	if cfg := s.collector.ConfigView(); cfg != nil && cfg.SourcePath != "" {
		out.Configurable, out.ConfigPath = true, cfg.SourcePath
		names := make([]string, 0, len(cfg.Remotes))
		for name := range cfg.Remotes {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			out.Remotes = append(out.Remotes, AccountSummary{
				Name: name, Type: cfg.Remotes[name].Type,
				HasCredentials: hasStoredCredential(cfg.Remotes[name]),
			})
		}
	}
	writeJSON(w, out)
}

func (s *Server) addAccount(w http.ResponseWriter, r *http.Request) {
	cfg := s.collector.ConfigView()
	if cfg == nil || cfg.SourcePath == "" {
		httpErrorT(w, r, http.StatusConflict, "err.no_config_for_remote")
		return
	}
	var in AddAccountRequest
	if !decodeMutation(w, r, &in) {
		return
	}
	remote, err := buildAccount(in)
	if err == nil && s.auth != nil && s.auth.FillClientID != nil {
		// Written now, while the account is being created, so its effective
		// binding is complete from the start; resolving it later would move
		// the binding and fence uploads already queued under the old one.
		s.auth.FillClientID(&remote)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	opt := config.AddRemoteOptions{MountPath: in.Mount, Prefix: in.Prefix, Mode: config.Mode(in.Mode), Pool: in.Pool}
	if opt.MountPath == "" && (opt.Prefix != "" || opt.Mode != "") {
		httpErrorT(w, r, http.StatusBadRequest, "err.prefix_needs_mount")
		return
	}
	if in.Pool != "" {
		if _, ok := cfg.Pools[in.Pool]; !ok {
			http.Error(w, fmt.Sprintf("unknown pool %q", in.Pool), http.StatusBadRequest)
			return
		}
	}
	// The account and its membership are one edit: an account that exists and
	// belongs to nothing is a state nobody asked for, and the reply used to
	// have to admit to producing it.
	if err := config.AddRemote(cfg.SourcePath, in.Name, remote, opt); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	joined := in.Pool
	s.reloadConfigView()
	writeJSON(w, AddAccountResponse{
		Name: in.Name, Type: in.Type,
		NextCommand:     fmt.Sprintf("cloudfs config auth %s --config %s", in.Name, cfg.SourcePath),
		Credentials:     credentialSummary(LangFrom(r), in.Type),
		RestartRequired: true,
		JoinedPool:      joined,
	})
}

// buildAccount validates the request into a remote. Every rejection here is a
// value that would otherwise be written to a configuration file by an HTTP
// request, so the checks are deliberately narrow.
func buildAccount(in AddAccountRequest) (config.Remote, error) {
	if !safeRemoteName(in.Name) {
		return config.Remote{}, errors.New("remote name must be 1..64 characters of letters, digits, dash or underscore")
	}
	known := false
	for _, t := range provider.Types() {
		if in.Type == t {
			known = true
		}
	}
	if !known {
		return config.Remote{}, fmt.Errorf("unknown backend type %q", in.Type)
	}
	if err := rejectSecretFields(in.Name, in.Fields); err != nil {
		return config.Remote{}, err
	}
	remote := config.Remote{Type: in.Type, Extra: map[string]any{}}
	for key, value := range in.Fields {
		if !config.SafeExtraFieldName(key) {
			return config.Remote{}, fmt.Errorf("invalid setting name %q", key)
		}
		if len(value) > 4096 || strings.ContainsAny(value, "\x00\r\n") {
			return config.Remote{}, fmt.Errorf("value for %q is too long or contains a control character", key)
		}
		if value == "" {
			continue
		}
		remote.Extra[key] = value
	}
	for _, f := range provider.Fields(in.Type) {
		if !f.Required {
			continue
		}
		if v, ok := remote.Extra[f.Name].(string); !ok || v == "" {
			return config.Remote{}, fmt.Errorf("%s is required for a %s remote", f.Name, in.Type)
		}
	}
	return remote, nil
}

func safeRemoteName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}
