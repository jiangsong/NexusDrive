package config

import (
	"errors"
	"fmt"
	"path"
	"strings"

	"gopkg.in/yaml.v3"
)

// validMountPrefix is the one rule for a layout prefix: absolute, cleaned,
// no backslash or NUL.
func validMountPrefix(prefix string) error {
	if !strings.HasPrefix(prefix, "/") || prefix != path.Clean(prefix) || strings.ContainsAny(prefix, "\\\x00") || containsControl(prefix) {
		return errors.New("config: mount prefix must be absolute and normalized")
	}
	if len(prefix) > 4096 {
		return errors.New("config: mount prefix is too long")
	}
	return nil
}

// sameMountPath reports whether two spellings name the same mount.
//
// The editors match against the raw YAML node, which holds whatever the person
// wrote — the setup wizard's default is the literal "~/CloudFS". Parse expands
// that path, so every caller that read the configuration hands back the
// expanded form. Comparing the two directly finds nothing: SetLayout and
// RemoveMount then report "no mount at /home/you/CloudFS" for a mount that is
// right there, and AddMount, which creates on miss, appends a SECOND entry for
// the same directory — and cloudfs mount only ever mounts Mounts[0], so the
// drive just bound in the browser silently never appears. Canonicalizing both
// sides fixes all three; the node keeps the ~ so the file stays portable.
func sameMountPath(a, b string) bool {
	return ExpandHome(a) == ExpandHome(b)
}

// upsertMountLayout finds the mount at mountPath — creating it when create is
// set — and installs layout under prefix. replace says whether an existing
// prefix is overwritten or refused. AddRemote, AddMount and SetLayout all go
// through here, so "find or create a mount, find or create a layout entry"
// has one implementation.
func upsertMountLayout(root *yaml.Node, mountPath, prefix string, layout Layout, create, replace bool) error {
	if err := validMountPrefix(prefix); err != nil {
		return err
	}
	mounts := mappingValue(root, "mounts")
	if mounts == nil || mounts.Tag == "!!null" {
		if !create {
			return fmt.Errorf("config: no mount at %s", mountPath)
		}
		mounts = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		setNode(root, "mounts", mounts)
	}
	for _, m := range mounts.Content {
		p := mappingValue(m, "path")
		if p == nil || !sameMountPath(p.Value, mountPath) {
			continue
		}
		ln := mappingValue(m, "layout")
		if ln == nil || ln.Kind != yaml.MappingNode {
			if ln != nil && ln.Tag != "!!null" {
				return errors.New("config: mount layout must be a mapping")
			}
			ln = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			setNode(m, "layout", ln)
		}
		if existing := mappingValue(ln, prefix); existing != nil && !replace {
			return fmt.Errorf("config: mount prefix %s already exists", prefix)
		} else if existing == nil && replace {
			return fmt.Errorf("config: no layout %s at %s", prefix, mountPath)
		}
		var entry yaml.Node
		if err := entry.Encode(layout); err != nil {
			return err
		}
		setNode(ln, prefix, &entry)
		return nil
	}
	if !create {
		return fmt.Errorf("config: no mount at %s", mountPath)
	}
	var m yaml.Node
	if err := m.Encode(Mount{Path: mountPath, Layout: map[string]Layout{prefix: layout}}); err != nil {
		return err
	}
	mounts.Content = append(mounts.Content, &m)
	return nil
}

func checkLayout(c *Config, layout Layout) error {
	if _, ok := c.Remotes[layout.Remote]; !ok {
		return fmt.Errorf("config: unknown remote %q", layout.Remote)
	}
	switch layout.Mode {
	case "", ModeWriteback, ModeStrict, ModeReadonly:
	default:
		return fmt.Errorf("config: unknown mode %q", layout.Mode)
	}
	return nil
}

// AddMount attaches a remote under prefix at mountPath, creating the mount
// entry when it does not exist yet. An existing prefix is refused: replacing
// what a path shows is SetLayout's job, said explicitly.
func AddMount(configPath, mountPath, prefix string, layout Layout) error {
	if strings.TrimSpace(mountPath) == "" || containsControl(mountPath) {
		return errors.New("config: mount path is required and must be a single-line value without control characters")
	}
	if len(mountPath) > 4096 {
		return errors.New("config: mount path is too long")
	}
	if layout.Mode == "" {
		layout.Mode = ModeWriteback
	}
	return editConfig(configPath, true, func(root *yaml.Node, c *Config) error {
		if err := checkLayout(c, layout); err != nil {
			return err
		}
		return upsertMountLayout(root, mountPath, prefix, layout, true, false)
	})
}

// SetLayout replaces the layout under an existing prefix.
func SetLayout(configPath, mountPath, prefix string, layout Layout) error {
	if layout.Mode == "" {
		layout.Mode = ModeWriteback
	}
	return editConfig(configPath, false, func(root *yaml.Node, c *Config) error {
		if err := checkLayout(c, layout); err != nil {
			return err
		}
		return upsertMountLayout(root, mountPath, prefix, layout, false, true)
	})
}

// RemoveMount removes one layout prefix; when it was the last one under the
// mount, the mount entry goes with it, since a mount that shows nothing is
// not a configuration anyone wrote on purpose.
func RemoveMount(configPath, mountPath, prefix string) error {
	return editConfig(configPath, false, func(root *yaml.Node, c *Config) error {
		mounts := mappingValue(root, "mounts")
		if mounts == nil || mounts.Kind != yaml.SequenceNode {
			return fmt.Errorf("config: no mount at %s", mountPath)
		}
		for i, m := range mounts.Content {
			p := mappingValue(m, "path")
			if p == nil || !sameMountPath(p.Value, mountPath) {
				continue
			}
			ln := mappingValue(m, "layout")
			if ln == nil || mappingValue(ln, prefix) == nil {
				return fmt.Errorf("config: no layout %s at %s", prefix, mountPath)
			}
			removeNode(ln, prefix)
			if len(ln.Content) == 0 {
				mounts.Content = append(mounts.Content[:i], mounts.Content[i+1:]...)
			}
			return nil
		}
		return fmt.Errorf("config: no mount at %s", mountPath)
	})
}
