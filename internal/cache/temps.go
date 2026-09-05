package cache

import (
	"errors"
	"os"
	"strings"
)

func validFileHash(s string) bool {
	if len(s) != 32 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func cacheTempName(s string) bool {
	if strings.HasPrefix(s, ".hydrate-") || strings.HasPrefix(s, ".install-") || strings.HasPrefix(s, ".flush-") {
		return true
	}
	if !strings.HasSuffix(s, ".tmp") || len(s) < 36 || !validFileHash(s[:32]) {
		return false
	}
	tail := strings.TrimSuffix(s[32:], ".tmp")
	if tail == "" || tail == ".key" {
		return true
	}
	_, _, ok := parseBlockName(s[:32] + strings.TrimSuffix(tail, ".part"))
	return ok
}

// Only abandoned temporaries (startup leftovers or failed cleanup) are eligible.
// Live conversions carry a reservation and are never in this map.
func (c *Cache) cleanTempLocked() bool {
	for p, size := range c.orphanTemps {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			continue
		}
		delete(c.orphanTemps, p)
		c.orphanBytes -= size
		c.evictions++
		return true
	}
	return false
}

// discardTemp cleans up an unpublished file after its writer has stopped.
// A failed unlink remains visible to accounting and can be retried by GC.
func (c *Cache) discardTemp(p string) {
	if err := os.Remove(p); err == nil || errors.Is(err, os.ErrNotExist) {
		return
	}
	info, err := os.Stat(p)
	if err != nil || !info.Mode().IsRegular() {
		return
	}
	c.mu.Lock()
	c.orphanBytes += info.Size() - c.orphanTemps[p]
	c.orphanTemps[p] = info.Size()
	c.mu.Unlock()
}
