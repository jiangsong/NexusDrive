package provider

import (
	"fmt"
	"sort"
	"sync"
)

// Factory builds a Provider from its config block (the `remotes.<name>` map
// minus `type`, `proxy` and `qps`, which the caller handles).
type Factory func(name string, cfg map[string]any) (Provider, error)

var (
	regMu    sync.RWMutex
	registry = map[string]Factory{}
)

// Register makes a backend type available to New. Backends call it from init.
func Register(typ string, f Factory) {
	regMu.Lock()
	defer regMu.Unlock()
	if _, dup := registry[typ]; dup {
		panic("provider: duplicate registration for " + typ)
	}
	registry[typ] = f
}

// New constructs the backend registered under typ.
func New(typ, name string, cfg map[string]any) (Provider, error) {
	regMu.RLock()
	f, ok := registry[typ]
	regMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("provider: unknown type %q", typ)
	}
	return f(name, cfg)
}

// Types lists registered backend types, sorted.
func Types() []string {
	regMu.RLock()
	defer regMu.RUnlock()
	out := make([]string, 0, len(registry))
	for t := range registry {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}
