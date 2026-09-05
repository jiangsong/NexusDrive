package config

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zalando/go-keyring"
	"golang.org/x/sys/unix"
	"gopkg.in/yaml.v3"
)

type Secrets struct {
	Backend string `yaml:"backend"` // auto, keyring, or file
	Dir     string `yaml:"dir"`
}

type SecretStore struct {
	dir, backend, service string
	get                   func(string, string) (string, error)
	set                   func(string, string, string) error
}

func NewSecretStore(c *Config) *SecretStore {
	dir := ExpandHome(c.Secrets.Dir)
	if dir == "" {
		if c.SourcePath != "" {
			dir = filepath.Join(filepath.Dir(c.SourcePath), "secrets")
		} else {
			dir = filepath.Join(c.Cache.Dir, "secrets")
		}
	}
	abs, _ := filepath.Abs(dir)
	return &SecretStore{dir: abs, backend: c.Secrets.Backend, service: fmt.Sprintf("cloudfs-%x", sha256.Sum256([]byte(abs))), get: keyring.Get, set: keyring.Set}
}

func IsSecretField(field string) bool {
	switch field {
	case "refresh_token", "access_token", "client_secret", "cookie", "password", "pass", "key_passphrase", "secret_access_key", "session_token":
		return true
	}
	return false
}

func secretReference(v string) bool {
	return strings.HasPrefix(v, "keyring:") || strings.HasPrefix(v, "secretfile:")
}

// IsSecretReference reports the two explicit storage reference schemes.
func IsSecretReference(v string) bool { return secretReference(v) }

func validSecret(key, value string) error {
	if key == "" || len(key) > 512 || strings.ContainsAny(key, "\x00\r\n") {
		return errors.New("config: invalid credential key")
	}
	if value == "" || len(value) > 1<<20 {
		return errors.New("config: credential must contain 1..1048576 bytes")
	}
	return nil
}

func (s *SecretStore) file(key string) string {
	return filepath.Join(s.dir, fmt.Sprintf("%x", sha256.Sum256([]byte(key))))
}

func (s *SecretStore) Get(ref string) (string, error) {
	kind, key, ok := strings.Cut(ref, ":")
	if !ok || key == "" {
		return "", errors.New("config: invalid credential reference")
	}
	switch kind {
	case "keyring":
		v, err := s.get(s.service, key)
		if err != nil {
			return "", fmt.Errorf("config: credential %s unavailable; run cloudfs config auth: %w", key, err)
		}
		return v, nil
	case "secretfile":
		fd, err := unix.Open(s.file(key), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil {
			return "", fmt.Errorf("config: credential %s unavailable; run cloudfs config auth: %w", key, err)
		}
		f := os.NewFile(uintptr(fd), s.file(key))
		defer f.Close()
		st, err := f.Stat()
		if err != nil {
			return "", err
		}
		if !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 {
			return "", errors.New("config: credential file must be a regular file with mode 0600")
		}
		if st.Size() > 1<<20 {
			return "", errors.New("config: credential exceeds size limit")
		}
		b := make([]byte, st.Size())
		if _, err := f.ReadAt(b, 0); err != nil {
			return "", err
		}
		return string(b), nil
	default:
		return "", errors.New("config: unknown credential reference")
	}
}

// Put prefers the system keyring. Auto mode records a distinct file reference
// when unavailable, so future reads never silently switch credential sources.
func (s *SecretStore) Put(key, value string) (string, error) {
	if err := validSecret(key, value); err != nil {
		return "", err
	}
	if s.backend != "file" {
		// The macOS implementation has a 4096-byte command limit.
		var err error
		if len(value) > 2000 {
			err = keyring.ErrSetDataTooBig
		} else {
			err = s.set(s.service, key, value)
		}
		if err == nil {
			return "keyring:" + key, nil
		}
		if s.backend == "keyring" {
			return "", fmt.Errorf("config: keyring save failed: %w", err)
		}
	}
	if err := os.MkdirAll(s.dir, 0700); err != nil {
		return "", err
	}
	if err := atomicPrivateWrite(s.file(key), []byte(value)); err != nil {
		return "", err
	}
	return "secretfile:" + key, nil
}

func (s *SecretStore) Update(ref, value string) error {
	_, key, _ := strings.Cut(ref, ":")
	if err := validSecret(key, value); err != nil {
		return err
	}
	if strings.HasPrefix(ref, "keyring:") {
		if len(value) > 2000 {
			return keyring.ErrSetDataTooBig
		}
		return s.set(s.service, strings.TrimPrefix(ref, "keyring:"), value)
	}
	if strings.HasPrefix(ref, "secretfile:") {
		return atomicPrivateWrite(s.file(strings.TrimPrefix(ref, "secretfile:")), []byte(value))
	}
	return errors.New("config: invalid credential reference")
}

// ResolveRemote returns a copy; the parsed configuration keeps references.
func (s *SecretStore) ResolveRemote(r Remote) (Remote, error) {
	out := r
	out.Extra = map[string]any{}
	for k, v := range r.Extra {
		if text, ok := v.(string); ok && secretReference(text) {
			resolved, err := s.Get(text)
			if err != nil {
				return Remote{}, err
			}
			out.Extra[k] = resolved
		} else {
			out.Extra[k] = v
		}
	}
	return out, nil
}

func atomicPrivateWrite(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".cloudfs-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err = f.Write(b); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// UpdateRemoteFields preserves YAML comments and unrelated settings, and
// serializes read/modify/rename across concurrent credential rotations.
func UpdateRemoteFields(path, name string, fields map[string]string) error {
	path = ExpandHome(path)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
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
	if err != nil {
		return err
	}
	var doc yaml.Node
	if err = yaml.Unmarshal(b, &doc); err != nil {
		return err
	}
	if len(doc.Content) != 1 {
		return errors.New("config: empty YAML")
	}
	root := doc.Content[0]
	remotes := mappingValue(root, "remotes")
	if remotes == nil {
		return errors.New("config: no remotes mapping")
	}
	remote := mappingValue(remotes, name)
	if remote == nil {
		return fmt.Errorf("config: remote %q does not exist", name)
	}
	if remote.Kind != yaml.MappingNode {
		return errors.New("config: remote must be a mapping")
	}
	for k, v := range fields {
		n := mappingValue(remote, k)
		if n == nil {
			n = &yaml.Node{}
			remote.Content = append(remote.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k}, n)
		}
		n.Kind = yaml.ScalarNode
		n.Tag = "!!str"
		n.Value = v
		n.Content = nil
	}
	b, err = yaml.Marshal(&doc)
	if err != nil {
		return err
	}
	return atomicPrivateWrite(path, b)
}

func mappingValue(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}
