package config

import (
	"errors"
	"reflect"
	"sync"

	"gopkg.in/yaml.v3"
)

var ErrCredentialsChanged = errors.New("config: remote credentials or settings changed on disk; restart the daemon before using this account")

func cloneRemote(r Remote) Remote {
	out := r
	out.Extra = make(map[string]any, len(r.Extra))
	for k, v := range r.Extra {
		out.Extra[k] = v
	}
	return out
}

// TokenPersister serializes rotation with config auth. A stale daemon must not
// replace references installed by a new authorization or a changed account.
// The callback owns its snapshots and is safe for concurrent provider calls.
func TokenPersister(c *Config, name string) func(map[string]string) error {
	expected := cloneRemote(c.Remotes[name])
	initialBinding, bindingErr := EffectiveAccountBinding(expected)
	var candidate *Remote
	var mu sync.Mutex
	return func(fields map[string]string) error {
		mu.Lock()
		defer mu.Unlock()
		if bindingErr != nil {
			return bindingErr
		}
		var next Remote
		err := editConfig(c.SourcePath, false, func(root *yaml.Node, current *Config) error {
			r, ok := current.Remotes[name]
			if !ok || (!reflect.DeepEqual(r, expected) && (candidate == nil || !reflect.DeepEqual(r, *candidate))) {
				return ErrCredentialsChanged
			}
			if !reflect.DeepEqual(c.Secrets, current.Secrets) {
				return ErrCredentialsChanged
			}
			next = cloneRemote(r)
			node := mappingValue(mappingValue(root, "remotes"), name)
			if node == nil || node.Kind != yaml.MappingNode {
				return ErrCredentialsChanged
			}
			if next.AccountBinding == "" {
				next.AccountBinding = initialBinding
				setNode(node, "account_binding", scalar(initialBinding))
			}
			store := NewSecretStore(current)
			for key, value := range fields {
				if !IsSecretField(key) {
					return errors.New("config: rotation contains a non-credential field")
				}
				if value == "" {
					continue
				}
				ref, _ := r.Extra[key].(string)
				if secretReference(ref) {
					if err := store.Update(ref, value); err != nil {
						return err
					}
				} else {
					var err error
					ref, err = store.Put(name+"/"+key, value)
					if err != nil {
						return err
					}
				}
				next.Extra[key] = ref
				setNode(node, key, scalar(ref))
			}
			// Keep a retry candidate even if fsync fails after rename. The
			// next attempt can retry durability without mistaking our own
			// visible references for another authorization.
			copy := cloneRemote(next)
			candidate = &copy
			return nil
		})
		if err == nil {
			expected = next
			candidate = nil
		}
		return err
	}
}
