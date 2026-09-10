package config

import (
	"errors"
	"fmt"
	"sort"
	"strconv"

	"gopkg.in/yaml.v3"
)

// RemoteMountedError says that removing a remote would leave a mount layout
// pointing at nothing. Callers may use the fields to render the refusal in the
// reader's language without parsing an English config error.
type RemoteMountedError struct {
	Remote string
	Path   string
	Prefix string
}

func (e *RemoteMountedError) Error() string {
	return fmt.Sprintf("config: remote %q is still mounted at %s%s; remove that layout first", e.Remote, e.Path, e.Prefix)
}

// RemotePoolMemberError says that a pool still owns the remote. Removing the
// account cannot silently remove the member: it may hold the pool's only copy
// of data and must go through the explicit drain/remove workflow first.
type RemotePoolMemberError struct {
	Remote string
	Pool   string
}

func (e *RemotePoolMemberError) Error() string {
	return fmt.Sprintf("config: remote %q is still a member of pool %q; drain and remove it from the pool first", e.Remote, e.Pool)
}

// The editors in this file exist so that a settings page can change an
// account without anyone opening the YAML in an editor over SSH. They all go
// through editConfig: one lock, node surgery that keeps the person's own
// comments, a full re-parse, an atomic rename. None of them touches a
// credential — that is config auth's job, and the field checks here refuse a
// secret key before anything is written.

// egressNames is every name a remote's proxy field or a rule may target:
// direct, each outbound, each group. Validate and the editors share it so
// they cannot disagree about what "unknown proxy" means.
func (c *Config) egressNames() map[string]bool {
	names := map[string]bool{"direct": true}
	for _, o := range c.Proxy.Outbounds {
		names[o.Name] = true
	}
	for _, g := range c.Proxy.Groups {
		names[g.Name] = true
	}
	return names
}

// RemoveRemote deletes an account block. It refuses while a mount layout or a
// storage pool still points at the remote, so the file can never be left with
// a dangling reference. Credentials stored for the remote are not deleted
// here: they are held under references unique to this authorization, and
// removing them is a separate, visible step.
func RemoveRemote(configPath, name string) error {
	return editConfig(configPath, false, func(root *yaml.Node, c *Config) error {
		if _, ok := c.Remotes[name]; !ok {
			return fmt.Errorf("config: unknown remote %q", name)
		}
		for _, m := range c.Mounts {
			for prefix, l := range m.Layout {
				if l.Remote == name {
					return &RemoteMountedError{Remote: name, Path: m.Path, Prefix: prefix}
				}
			}
		}
		// Sort for deterministic refusals even if a malformed config somehow
		// reaches this editor with the same member in more than one pool.
		pools := make([]string, 0, len(c.Pools))
		for pool := range c.Pools {
			pools = append(pools, pool)
		}
		sort.Strings(pools)
		for _, pool := range pools {
			for _, member := range c.Pools[pool].Members {
				if member.Remote == name {
					return &RemotePoolMemberError{Remote: name, Pool: pool}
				}
			}
		}
		remotes := mappingValue(root, "remotes")
		if remotes == nil {
			return fmt.Errorf("config: unknown remote %q", name)
		}
		removeNode(remotes, name)
		return nil
	})
}

// SetRemoteFieldOptions is a partial update of one account. A nil pointer
// leaves that setting alone. For Fields, a nil value — or a pointer to the
// empty string — deletes the key; any other value sets it. Proxy "" and
// UploadWorkers 0 likewise remove their keys, returning the setting to the
// default. There is deliberately no way to set an extra field to the empty
// string: an empty scalar and an absent key mean the same thing to the drivers.
type SetRemoteFieldOptions struct {
	Fields        map[string]*string
	Proxy         *string
	QPS           *QPS
	UploadWorkers *int
}

// SetRemoteField changes the public settings of an existing account.
func SetRemoteField(configPath, name string, opt SetRemoteFieldOptions) error {
	for k, v := range opt.Fields {
		if IsSecretField(k) {
			return fmt.Errorf("config: %s is a credential; set it with config auth", k)
		}
		if !SafeExtraFieldName(k) {
			return fmt.Errorf("config: reserved or invalid remote field %s", k)
		}
		if v != nil && (len(*v) > 4096 || containsControl(*v)) {
			return fmt.Errorf("config: value for %s is too long or contains a control character", k)
		}
	}
	if opt.UploadWorkers != nil && (*opt.UploadWorkers < 0 || *opt.UploadWorkers > 256) {
		return errors.New("config: upload_workers must be 0..256")
	}
	if opt.QPS != nil && (opt.QPS.Meta < 0 || opt.QPS.Download < 0 || opt.QPS.Upload < 0) {
		return errors.New("config: qps values must not be negative")
	}
	return editConfig(configPath, false, func(root *yaml.Node, c *Config) error {
		if _, ok := c.Remotes[name]; !ok {
			return fmt.Errorf("config: unknown remote %q", name)
		}
		if opt.Proxy != nil && *opt.Proxy != "" && !c.egressNames()[*opt.Proxy] {
			return fmt.Errorf("config: unknown proxy %q; define the outbound or group first", *opt.Proxy)
		}
		remotes := mappingValue(root, "remotes")
		remote := mappingValue(remotes, name)
		if remote == nil || remote.Kind != yaml.MappingNode {
			return fmt.Errorf("config: remote %q is not a mapping", name)
		}
		for k, v := range opt.Fields {
			if v == nil || *v == "" {
				removeNode(remote, k)
			} else {
				setNode(remote, k, scalar(*v))
			}
		}
		if opt.Proxy != nil {
			if *opt.Proxy == "" {
				removeNode(remote, "proxy")
			} else {
				setNode(remote, "proxy", scalar(*opt.Proxy))
			}
		}
		if opt.QPS != nil {
			if *opt.QPS == (QPS{}) {
				removeNode(remote, "qps")
			} else {
				var n yaml.Node
				if err := n.Encode(*opt.QPS); err != nil {
					return err
				}
				setNode(remote, "qps", &n)
			}
		}
		if opt.UploadWorkers != nil {
			if *opt.UploadWorkers == 0 {
				removeNode(remote, "upload_workers")
			} else {
				setNode(remote, "upload_workers", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.Itoa(*opt.UploadWorkers)})
			}
		}
		return nil
	})
}

func containsControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}
