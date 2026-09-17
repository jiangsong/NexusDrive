package hooks

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Hermes keeps its hooks in YAML as flat lists: hooks.<event> is a list
// of {matcher?, command, timeout} entries, one per hook, with no group
// around them (UNVERIFIED, see platforms). The same idempotency as the
// JSON shape: our entries carry the marker in their command, are
// converged per kind, and are the only ones uninstall touches. The file
// is decoded to plain maps and encoded again, so comments do not survive
// an install; the person is told (Result.Note) and the backup keeps them.

// yamlEntries builds our flat entries for a client.
func yamlEntries(client string) []struct {
	event, kind string
	entry       map[string]any
} {
	p := platforms[client]
	entry := func(kind, matcher string) map[string]any {
		e := map[string]any{"command": Command(kind, client), "timeout": p.timeout}
		if matcher != "" {
			e["matcher"] = matcher
		}
		return e
	}
	return []struct {
		event, kind string
		entry       map[string]any
	}{
		{p.prompt, "prompt", entry("prompt", "")},
		{p.tool, "read", entry("read", p.readMatcher)},
		{p.tool, "write", entry("write", p.writeMatcher)},
		{p.stop, "stop", entry("stop", "")},
	}
}

func readYAML(path string) (map[string]any, error) {
	root := map[string]any{}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return root, nil
	}
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return root, nil
	}
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if root == nil {
		root = map[string]any{}
	}
	return root, nil
}

func writeYAML(path string, root map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := yaml.Marshal(root)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// mergeIntoYAML adds or converges our entries in the YAML file at path.
func mergeIntoYAML(path, client string) (bool, error) {
	root, err := readYAML(path)
	if err != nil {
		return false, err
	}
	hooks, ok := root["hooks"].(map[string]any)
	if !ok {
		hooks = map[string]any{}
		root["hooks"] = hooks
	}
	changed := false
	current := map[string]bool{}
	for _, e := range yamlEntries(client) {
		current[e.event] = true
		arr, _ := hooks[e.event].([]any)
		if i := indexOfOurs(arr, e.kind); i >= 0 {
			if !jsonEqual(arr[i], e.entry) {
				arr[i] = e.entry
				hooks[e.event] = arr
				changed = true
			}
			continue
		}
		hooks[e.event] = append(arr, e.entry)
		changed = true
	}
	for event := range ourEvents {
		if current[event] {
			continue
		}
		arr, ok := hooks[event].([]any)
		if !ok {
			continue
		}
		kept := arr[:0:0]
		for _, g := range arr {
			if containsMarker(g) {
				changed = true
				continue
			}
			kept = append(kept, g)
		}
		if len(kept) == 0 {
			delete(hooks, event)
		} else {
			hooks[event] = kept
		}
	}
	if !changed {
		return false, nil
	}
	return true, writeYAML(path, root)
}

// removeFromYAML drops our entries from the YAML file at path.
func removeFromYAML(path string) (bool, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	root, err := readYAML(path)
	if err != nil {
		return false, err
	}
	hooks, ok := root["hooks"].(map[string]any)
	if !ok {
		return false, nil
	}
	changed := false
	for event := range ourEvents {
		arr, ok := hooks[event].([]any)
		if !ok {
			continue
		}
		var kept []any
		for _, g := range arr {
			if containsMarker(g) {
				changed = true
				continue
			}
			kept = append(kept, g)
		}
		if len(kept) == 0 {
			delete(hooks, event)
		} else {
			hooks[event] = kept
		}
	}
	if !changed {
		return false, nil
	}
	if len(hooks) == 0 {
		delete(root, "hooks")
	}
	return true, writeYAML(path, root)
}
