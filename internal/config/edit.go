package config

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"unicode"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
	"gopkg.in/yaml.v3"
)

// editConfig serializes edits without rewriting unrelated YAML nodes. The
// complete result is validated before its atomic rename becomes visible.
func editConfig(path string, create bool, edit func(*yaml.Node, *Config) error) error {
	path, err := filepath.Abs(ExpandHome(path))
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	fd, err := unix.Open(path+".lock", unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), path+".lock")
	defer f.Close()
	if err = unix.Flock(fd, unix.LOCK_EX); err != nil {
		return err
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) && create {
		b = []byte("remotes: {}\nmounts: []\n")
		err = nil
	}
	if err != nil {
		return err
	}
	c, err := Parse(b)
	if err != nil {
		return err
	}
	c.SourcePath = path
	var doc yaml.Node
	if err = yaml.Unmarshal(b, &doc); err != nil {
		return err
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return errors.New("config: expected a YAML mapping")
	}
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

// AddRemote creates an account without overwriting an existing one. MountPath
// optionally attaches it to a mount in the same atomic configuration edit.
type AddRemoteOptions struct {
	MountPath, Prefix, Root string
	Mode                    Mode
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
		if k == "type" || k == "proxy" || k == "qps" || k == "upload_workers" || strings.HasPrefix(k, "_") {
			return fmt.Errorf("config: reserved remote field %s", k)
		}
		if IsSecretField(k) && v != "" {
			return fmt.Errorf("config: set %s through config auth, not config add", k)
		}
	}
	return editConfig(configPath, true, func(root *yaml.Node, c *Config) error {
		if _, exists := c.Remotes[name]; exists {
			return fmt.Errorf("config: remote %q already exists", name)
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
			if !strings.HasPrefix(prefix, "/") || prefix != path.Clean(prefix) || strings.ContainsAny(prefix, "\\\x00") {
				return errors.New("config: mount prefix must be absolute and normalized")
			}
			mode := opt.Mode
			if mode == "" {
				mode = ModeWriteback
			}
			layout := Layout{Remote: name, Root: opt.Root, Mode: mode}
			mounts := mappingValue(root, "mounts")
			if mounts == nil || mounts.Tag == "!!null" {
				mounts = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
				setNode(root, "mounts", mounts)
			}
			for _, m := range mounts.Content {
				if p := mappingValue(m, "path"); p != nil && p.Value == opt.MountPath {
					ln := mappingValue(m, "layout")
					if ln == nil || ln.Kind != yaml.MappingNode {
						return errors.New("config: mount layout must be a mapping")
					}
					if mappingValue(ln, prefix) != nil {
						return fmt.Errorf("config: mount prefix %s already exists", prefix)
					}
					var entry yaml.Node
					if err := entry.Encode(layout); err != nil {
						return err
					}
					setNode(ln, prefix, &entry)
					return nil
				}
			}
			var m yaml.Node
			if err := m.Encode(Mount{Path: opt.MountPath, Layout: map[string]Layout{prefix: layout}}); err != nil {
				return err
			}
			mounts.Content = append(mounts.Content, &m)
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
