package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"unicode"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
)

// editConfig serializes edits without rewriting unrelated YAML nodes. The
// complete result is validated before its atomic rename becomes visible.
func editConfig(path string, create bool, edit func(*yaml.Node, *Config) error) error {
	return editConfigNode(path, create, nil, edit)
}

// editConfigNode is editConfig with a pre-parse hook: prepare runs on the
// raw document before it is parsed, for an edit whose whole point is to
// make a file valid that is not yet (adding the api_key an openai block
// requires). The prepared document is what edit sees and what is
// validated again before the write.
func editConfigNode(path string, create bool, prepare func(*yaml.Node) error, edit func(*yaml.Node, *Config) error) error {
	path, err := filepath.Abs(ExpandHome(path))
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := openLockedFile(path + ".lock")
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) && create {
		b = []byte("remotes: {}\nmounts: []\n")
		err = nil
	}
	if err != nil {
		return err
	}
	var doc yaml.Node
	if err = yaml.Unmarshal(b, &doc); err != nil {
		return err
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return errors.New("config: expected a YAML mapping")
	}
	if prepare != nil {
		if err = prepare(doc.Content[0]); err != nil {
			return err
		}
		if b, err = yaml.Marshal(&doc); err != nil {
			return err
		}
	}
	c, err := Parse(b)
	if err != nil {
		return err
	}
	c.SourcePath = path
	if err = edit(doc.Content[0], c); err != nil {
		return err
	}
	b, err = yaml.Marshal(&doc)
	if err != nil {
		return err
	}
	if _, err = Parse(b); err != nil {
		return err
	}
	return atomicPrivateWrite(path, b)
}

func setNode(n *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			value.HeadComment = n.Content[i+1].HeadComment
			value.LineComment = n.Content[i+1].LineComment
			value.FootComment = n.Content[i+1].FootComment
			*n.Content[i+1] = *value
			return
		}
	}
	n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, value)
}

func scalar(value string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
}

func removeNode(n *yaml.Node, key string) {
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			n.Content = append(n.Content[:i], n.Content[i+2:]...)
			return
		}
	}
}

// SafeExtraFieldName reports whether name may be set as a public field of a
// remote by a caller that did not write the YAML itself: the CLI's --set, the
// control plane's account endpoints, and the config editors here. It is the
// one definition of that rule, so the HTTP side and the file side cannot
// drift on which keys a request is allowed to write.
//
// Structural keys (type, proxy, qps, upload_workers) are set through their
// own typed paths, never through the free-form field map; keys starting with
// "_" are the daemon's injection slots (provider.ConfigHTTPClient and
// friends) and must never come from outside the process.
func SafeExtraFieldName(name string) bool {
	if name == "" || len(name) > 64 || strings.HasPrefix(name, "_") {
		return false
	}
	switch name {
	case "type", "proxy", "qps", "upload_workers", "account_binding":
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
		default:
			return false
		}
	}
	return true
}

// AddRemote creates an account without overwriting an existing one. MountPath
// optionally attaches it to a mount, and Pool optionally joins it to a storage
// pool, both in the same atomic configuration edit — an account that exists
// but belongs to nothing is a state nobody asked for.
type AddRemoteOptions struct {
	MountPath, Prefix, Root string
	Mode                    Mode
	// Pool names an existing storage pool to join. Creating the pool is not
	// part of adding an account: a pool decides how many copies of every file
	// its members hold, which is a choice of its own.
	Pool string
}

func AddRemote(configPath, name string, r Remote, opt AddRemoteOptions) error {
	if strings.TrimSpace(name) != name || name == "" || len(name) > 128 || strings.ContainsAny(name, "/\\") || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return errors.New("config: remote name must be nonempty and contain no path separators or control characters")
	}
	if r.Type == "" {
		return errors.New("config: remote type is required")
	}
	if r.AccountBinding == "" {
		r.AccountBinding = NewAccountBinding()
	} else if !ValidAccountBinding(r.AccountBinding) {
		return errors.New("config: remote account binding is invalid")
	}
	for k, v := range r.Extra {
		if !SafeExtraFieldName(k) {
			return fmt.Errorf("config: reserved or invalid remote field %s", k)
		}
		if IsSecretField(k) && v != "" {
			return fmt.Errorf("config: set %s through config auth, not config add", k)
		}
	}
	return editConfig(configPath, true, func(root *yaml.Node, c *Config) error {
		if _, exists := c.Remotes[name]; exists {
			return fmt.Errorf("config: remote %q already exists", name)
		}
		if opt.Pool != "" {
			// Checked before anything is written, so a typo reads as a
			// refusal rather than as a half-finished account.
			if _, ok := c.Pools[opt.Pool]; !ok {
				return fmt.Errorf("config: unknown pool %q", opt.Pool)
			}
		}
		remotes := mappingValue(root, "remotes")
		if remotes == nil || remotes.Tag == "!!null" {
			remotes = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			setNode(root, "remotes", remotes)
		}
		var n yaml.Node
		if err := n.Encode(r); err != nil {
			return err
		}
		setNode(remotes, name, &n)
		if opt.MountPath != "" {
			prefix := opt.Prefix
			if prefix == "" {
				prefix = "/" + name
			}
			mode := opt.Mode
			if mode == "" {
				mode = ModeWriteback
			}
			if err := upsertMountLayout(root, opt.MountPath, prefix, Layout{Remote: name, Root: opt.Root, Mode: mode}, true, false); err != nil {
				return err
			}
		}
		if opt.Pool != "" {
			if err := appendPoolMember(root, c, opt.Pool, PoolMember{Remote: name}, r.Type); err != nil {
				return err
			}
		}
		return nil
	})
}

// SaveCredentials saves newly authorized fields and migrates remaining inline
// secrets. Each new reference gets a unique key, so an interrupted config
// update cannot overwrite credentials still referenced by the old YAML.
func SaveCredentials(path, name string, fields map[string]string) (fileFallback bool, err error) {
	return saveCredentials(path, name, fields, nil, true)
}

// SaveCredentialsForRemote refuses to install an authorization if the remote
// was edited while the user was in the browser. Credential rotations that
// preserve existing references do not change the account configuration.
func SaveCredentialsForRemote(path, name string, fields map[string]string, expected Remote) (bool, error) {
	return saveCredentials(path, name, fields, &expected, true)
}

// SaveCredentialsForRemotePreservingBinding is used only when moving the
// same credentials from inline YAML into private storage. It persists a legacy
// generation before changing references, so existing uploads remain fenced to
// the same configuration. It must not be used for new authorization material.
func SaveCredentialsForRemotePreservingBinding(path, name string, fields map[string]string, expected Remote) (bool, error) {
	return saveCredentials(path, name, fields, &expected, false)
}

func saveCredentials(path, name string, fields map[string]string, expected *Remote, rotateBinding bool) (fileFallback bool, err error) {
	err = editConfig(path, false, func(root *yaml.Node, c *Config) error {
		r, ok := c.Remotes[name]
		if !ok {
			return fmt.Errorf("config: unknown remote %q", name)
		}
		if expected != nil && !reflect.DeepEqual(r, *expected) {
			return ErrCredentialsChanged
		}
		values := map[string]string{}
		for k, v := range r.Extra {
			if IsSecretField(k) {
				if text, ok := v.(string); ok && text != "" {
					values[k] = text
				}
			}
		}
		for k, v := range fields {
			if !IsSecretField(k) {
				return fmt.Errorf("config: %s is not a credential field", k)
			}
			if v == "" {
				return fmt.Errorf("config: %s must not be empty", k)
			}
			values[k] = v
		}
		// A new refresh token must not keep an old account's access token.
		_, replacingRefresh := fields["refresh_token"]
		_, replacingAccess := fields["access_token"]
		if replacingRefresh && !replacingAccess {
			delete(values, "access_token")
		}
		if replacingAccess && !replacingRefresh {
			delete(values, "refresh_token")
		}
		for key, value := range values {
			if err := validSecret(name+"/"+key, value); err != nil {
				return err
			}
		}
		store := NewSecretStore(c)
		// Verify all references before creating any new secret entries.
		for key, value := range values {
			if _, supplied := fields[key]; !supplied && secretReference(value) {
				if _, err := store.Get(value); err != nil {
					return err
				}
			}
		}
		keys := make([]string, 0, len(values))
		for k := range values {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		n := mappingValue(mappingValue(root, "remotes"), name)
		if n == nil || n.Kind != yaml.MappingNode {
			return errors.New("config: remote must be a mapping")
		}
		binding := r.AccountBinding
		if rotateBinding {
			binding = NewAccountBinding()
		} else if binding == "" {
			var err error
			binding, err = EffectiveAccountBinding(r)
			if err != nil {
				return err
			}
		}
		setNode(n, "account_binding", scalar(binding))
		if replacingRefresh && !replacingAccess {
			removeNode(n, "access_token")
		}
		if replacingAccess && !replacingRefresh {
			removeNode(n, "refresh_token")
		}
		for _, k := range keys {
			v := values[k]
			_, supplied := fields[k]
			if !secretReference(v) || supplied {
				ref, e := store.Put(name+"/"+k+"/"+uuid.NewString(), v)
				if e != nil {
					return e
				}
				v = ref
			}
			if strings.HasPrefix(v, "secretfile:") {
				fileFallback = true
			}
			setNode(n, k, scalar(v))
		}
		return nil
	})
	return fileFallback, err
}
