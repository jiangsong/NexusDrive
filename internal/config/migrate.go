package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"gopkg.in/yaml.v3"
)

// LegacyConfigPath and legacyCacheRoot are where an installation kept its
// files before the single root: the XDG split, with the configuration in one
// directory and everything else — the journal's unuploaded bytes included —
// in a directory the system treats as disposable.
const (
	LegacyConfigPath = "~/.config/cloudfs/config.yaml"
	legacyCacheRoot  = "~/.cache/cloudfs"
)

// legacyStateNames are the entries that move out of the old cache directory.
// "blocks" is deliberately absent: it is reproducible, it can be tens of
// gigabytes, and a rename that crosses a filesystem would turn a migration
// into a long copy. The migrated configuration names the old directory
// instead, so the blocks stay where they are and stay in use.
var legacyStateNames = []string{
	"meta.db", "index.db", "exports.db", "journal", "agent", "pool", "secrets",
}

// Migration reports what MigrateLegacyLayout did.
type Migration struct {
	// Ran is false when there was nothing to migrate.
	Ran bool
	// From is the legacy configuration that was moved.
	From string
	// To is the configuration in the new root.
	To string
	// Moved names the entries that were moved out of the legacy cache.
	Moved []string
	// BlocksKept is the directory the block cache was left in, which the
	// migrated configuration now names explicitly.
	BlocksKept string
}

// MigrateLegacyLayout moves a pre-single-root installation into ~/.cloudfs.
//
// It runs only when newPath is the default configuration path, there is no
// configuration there yet, and a legacy configuration exists. Any other
// combination returns Ran false and touches nothing: a configuration named by
// --config or CLOUDFS_CONFIG is a path chosen for one command, and moving a
// person's state underneath it would be a surprise.
//
// busy reports whether a daemon is answering on a control socket. A migration
// that renames a journal out from under a running daemon would leave it
// writing into an unlinked directory, so a live socket refuses the migration
// rather than racing it. A nil busy means the caller has already established
// that nothing is running.
//
// The order is what makes an interrupted run safe: every entry is moved only
// when the destination does not exist, and the configuration is written last.
// The presence of the new configuration is the "already migrated" flag, so a
// run that dies halfway is resumed by the next one rather than leaving state
// split between two roots.
func MigrateLegacyLayout(newPath string, busy func(socket string) bool) (Migration, error) {
	var m Migration
	want, err := filepath.Abs(ExpandHome(filepath.Join(DefaultRoot, "config.yaml")))
	if err != nil {
		return m, err
	}
	got, err := filepath.Abs(ExpandHome(newPath))
	if err != nil {
		return m, err
	}
	if got != want {
		return m, nil
	}
	if _, err := os.Stat(got); err == nil {
		return m, nil
	} else if !os.IsNotExist(err) {
		return m, err
	}
	legacy := ExpandHome(LegacyConfigPath)
	b, err := os.ReadFile(legacy)
	if os.IsNotExist(err) {
		return m, nil
	} else if err != nil {
		return m, err
	}
	old, err := Parse(b)
	if err != nil {
		return m, fmt.Errorf("config: the existing %s does not parse, so it was not migrated: %w", legacy, err)
	}
	legacyCache := old.Cache.Dir
	if legacyCache == "" {
		legacyCache = ExpandHome(legacyCacheRoot)
	}
	if busy != nil && busy(ExpandHome(old.Control.Socket)) {
		return m, fmt.Errorf("config: a daemon is running on %s; stop it before the files move", ExpandHome(old.Control.Socket))
	}
	root := filepath.Dir(got)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return m, err
	}
	for _, name := range legacyStateNames {
		moved, err := moveIfAbsent(filepath.Join(legacyCache, name), filepath.Join(root, name))
		if err != nil {
			return m, err
		}
		if moved {
			m.Moved = append(m.Moved, name)
		}
	}
	// The configuration is rewritten rather than renamed, because the block
	// cache has to be named explicitly before the file lands. A legacy
	// configuration that relied on the old default names no cache directory
	// at all; moved as-is, it would resolve to the new root and leave every
	// cached block orphaned in a directory nothing reads.
	body, err := withCachePaths(b, legacyCache, filepath.Join(root, "control.sock"))
	if err != nil {
		return m, err
	}
	if err := atomicPrivateWrite(got, body); err != nil {
		return m, err
	}
	if _, err := Load(got); err != nil {
		os.Remove(got)
		return m, fmt.Errorf("config: the migrated file did not validate: %w", err)
	}
	// Keep the original beside its old home rather than deleting it: it costs
	// nothing and it is the only copy of what the person had before.
	if err := os.Rename(legacy, legacy+".moved-to-cloudfs"); err != nil && !os.IsNotExist(err) {
		return m, err
	}
	m.Ran, m.From, m.To, m.BlocksKept = true, legacy, got, legacyCache
	return m, nil
}

// moveIfAbsent renames src to dst unless dst already exists, which is how an
// interrupted migration resumes without overwriting what it already moved.
func moveIfAbsent(src, dst string) (bool, error) {
	if _, err := os.Stat(src); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if _, err := os.Stat(dst); err == nil {
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, err
	}
	if err := os.Rename(src, dst); err != nil {
		var link *os.LinkError
		if errors.As(err, &link) && errors.Is(link.Err, syscall.EXDEV) {
			return false, fmt.Errorf("config: %s and %s are on different filesystems, so nothing was moved automatically; "+
				"copy it yourself and run again:\n  cp -a %s %s && rm -rf %s", src, dst, src, dst, src)
		}
		return false, err
	}
	return true, nil
}

// withCachePaths returns the configuration with cache.dir set to cacheDir,
// and control.socket repointed at socket when it still names a path inside
// the directory the state just left. Comments survive: the file is a person's
// to edit, and a migration that silently strips their notes is a bad trade.
func withCachePaths(b []byte, cacheDir, socket string) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("config: expected a YAML mapping")
	}
	rootNode := doc.Content[0]
	cache := childMapping(rootNode, "cache")
	setNode(cache, "dir", scalar(cacheDir))
	control := childMapping(rootNode, "control")
	if cur := childScalar(control, "socket"); cur != "" && within(ExpandHome(cur), cacheDir) {
		setNode(control, "socket", scalar(socket))
	}
	return yaml.Marshal(&doc)
}

// childMapping returns the mapping stored under key, creating an empty one
// when the document does not have it yet.
func childMapping(n *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key && n.Content[i+1].Kind == yaml.MappingNode {
			return n.Content[i+1]
		}
	}
	child := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	setNode(n, key, child)
	return child
}

func childScalar(n *yaml.Node, key string) string {
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1].Value
		}
	}
	return ""
}

// within reports whether p is dir itself or sits under it.
func within(p, dir string) bool {
	p, dir = filepath.Clean(p), filepath.Clean(dir)
	return p == dir || strings.HasPrefix(p, dir+string(filepath.Separator))
}
