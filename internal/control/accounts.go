package control

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"cloudfs/internal/config"
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
	// Credentials is what `config auth` will ask for, in plain words.
	Credentials string `json:"credentials,omitempty"`
}

// AccountSummary is a configured remote, without any credential value.
type AccountSummary struct {
	Name string `json:"name"`
	Type string `json:"type"`
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

func (c *Collector) accountTypes() []AccountType {
	types := provider.DescribedTypes()
	out := make([]AccountType, 0, len(types))
	for _, typ := range types {
		entry := AccountType{Type: typ, Credentials: credentialSummary(typ)}
		for _, f := range provider.Fields(typ) {
			entry.Fields = append(entry.Fields, AccountField{
				Name: f.Name, Prompt: f.Prompt, Required: f.Required,
				Default: f.Default, Example: f.Example,
			})
		}
		out = append(out, entry)
	}
	return out
}

func credentialSummary(typ string) string {
	creds := provider.CredentialsFor(typ)
	if creds.Note != "" {
		return creds.Note
	}
	return strings.Join(creds.Fields, " or ")
}

func (s *Server) accounts(w http.ResponseWriter, r *http.Request) {
	if !privateRequest(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.listAccounts(w)
	case http.MethodPost:
		s.addAccount(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) listAccounts(w http.ResponseWriter) {
	out := AccountsResponse{Types: s.collector.accountTypes()}
	if cfg := s.collector.Config; cfg != nil && cfg.SourcePath != "" {
		out.Configurable, out.ConfigPath = true, cfg.SourcePath
		names := make([]string, 0, len(cfg.Remotes))
		for name := range cfg.Remotes {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			out.Remotes = append(out.Remotes, AccountSummary{Name: name, Type: cfg.Remotes[name].Type})
		}
	}
	writeJSON(w, out)
}

func (s *Server) addAccount(w http.ResponseWriter, r *http.Request) {
	cfg := s.collector.Config
	if cfg == nil || cfg.SourcePath == "" {
		http.Error(w, "this daemon has no configuration file to add a remote to", http.StatusConflict)
		return
	}
	var in AddAccountRequest
	if !decodeMutation(w, r, &in) {
		return
	}
	remote, err := buildAccount(in)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	opt := config.AddRemoteOptions{MountPath: in.Mount, Prefix: in.Prefix, Mode: config.Mode(in.Mode)}
	if opt.MountPath == "" && (opt.Prefix != "" || opt.Mode != "") {
		http.Error(w, "prefix and mode require a mount path", http.StatusBadRequest)
		return
	}
	if in.Pool != "" {
		if _, ok := cfg.Pools[in.Pool]; !ok {
			http.Error(w, fmt.Sprintf("unknown pool %q", in.Pool), http.StatusBadRequest)
			return
		}
	}
	if err := config.AddRemote(cfg.SourcePath, in.Name, remote, opt); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	joined := ""
	if in.Pool != "" {
		if err := config.AddPoolMember(cfg.SourcePath, in.Pool, config.PoolMember{Remote: in.Name}); err != nil {
			http.Error(w, "remote written, but joining the pool failed: "+err.Error(), http.StatusConflict)
			return
		}
		joined = in.Pool
	}
	s.reloadConfigView()
	writeJSON(w, AddAccountResponse{
		Name: in.Name, Type: in.Type,
		NextCommand:     fmt.Sprintf("cloudfs config auth %s --config %s", in.Name, cfg.SourcePath),
		Credentials:     credentialSummary(in.Type),
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
