package cache

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// The key index is the one file that says which remote file each cached hash
// belongs to. A restart can hash a key into a directory name but not a
// directory name back into a key, and without the key the cache cannot say
// which file a block belongs to — so `cloudfs bench --cold` would drop nothing
// and measure a warm cache, `cache drop` would report files it did not touch,
// and the VFS could not re-pin the files whose only copy is the local one.
//
// It is one append-only file of JSON lines rather than a sidecar beside each
// file's blocks because the sidecars decided the shape of the blocks/ tree: a
// leaf directory per file ever cached, whole files included, which hold no
// block there at all. Reload walks that tree, and on a cold rotating disk each
// directory is a seek; a cache of twenty-five thousand files took minutes to
// reload before the mount appeared. The index is a single sequential read,
// and the tree holds only directories that hold blocks.
//
// Forgetting a file appends a tombstone rather than editing the file; the
// last line for a key wins on reload. Dead lines — tombstones and the records
// they cancel — stay behind until a compaction rewrites the index from the
// live set: on a reload that finds any, and while running once they outnumber
// keysCompactSlack. Appends are not fsynced, exactly as the sidecars were
// not: a crash may lose the last identity written, and the reload then knows
// the blocks but not their file.
const keysFileName = "keys"

// keysCompactSlack is how many lines beyond the live keys the running index
// may hold before it is rewritten. A variable so a test can churn cheaply.
var keysCompactSlack = 4096

type keyRecord struct {
	Remote   string `json:"r"`
	RemoteID string `json:"i"`
	Version  string `json:"v"`
	// Gone marks a tombstone: the file was forgotten after this record.
	Gone bool `json:"gone,omitempty"`
}

// keysIndex is the on-disk file behind Cache.files' identities. Its mutex
// orders appends with compaction; it is never taken under c.mu.
type keysIndex struct {
	mu sync.Mutex
	// lines counts the records in the file since the last rewrite, live or
	// dead, so compaction can be decided without reading it.
	lines int
	// retryAt is the line count a failed compaction waits for before trying
	// again: a rewrite is a pass over every live key under admitMu, and a
	// disk that refused it once will usually refuse it on the next append.
	retryAt int
	// compactions counts the rewrites attempted, for tests.
	compactions int
}

func (c *Cache) keysPath() string { return filepath.Join(c.opt.Dir, keysFileName) }

// loadKeys reads the index. Malformed lines — a torn append after a crash —
// are skipped; the cache is not made unusable by one bad line. It returns the
// number of lines the file held so reload knows whether a rewrite is due.
func (c *Cache) loadKeys() (map[string]FileKey, int, error) {
	f, err := os.Open(c.keysPath())
	if errors.Is(err, os.ErrNotExist) {
		return map[string]FileKey{}, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	known := map[string]FileKey{}
	lines := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		lines++
		var rec keyRecord
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil || rec.RemoteID == "" {
			continue
		}
		k := FileKey{Remote: rec.Remote, RemoteID: rec.RemoteID, Version: rec.Version}
		if rec.Gone {
			delete(known, k.hash())
			continue
		}
		known[k.hash()] = k
	}
	if err := sc.Err(); err != nil {
		return nil, 0, err
	}
	return known, lines, nil
}

// writeKeys replaces the index with exactly these keys, through a temporary
// so a reader never sees a half-written file.
func (c *Cache) writeKeys(keys []FileKey) error {
	p := c.keysPath()
	tmp := p + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	for _, k := range keys {
		if err := writeKeyRecord(w, k, false); err != nil {
			f.Close()
			os.Remove(tmp)
			return err
		}
	}
	if err := w.Flush(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
		return err
	}
	c.keys.lines = len(keys)
	return nil
}

func writeKeyRecord(w *bufio.Writer, k FileKey, gone bool) error {
	b, err := json.Marshal(keyRecord{Remote: k.Remote, RemoteID: k.RemoteID, Version: k.Version, Gone: gone})
	if err != nil {
		return err
	}
	if _, err := w.Write(b); err != nil {
		return err
	}
	return w.WriteByte('\n')
}

// rememberKey records a file's identity in the index. Callers decide under
// c.mu whether it is needed (fileState.keyWritten) and call this after
// releasing it, so the write never holds up a reader.
func (c *Cache) rememberKey(k FileKey) { c.appendKey(k, false) }

// forgetKey appends the tombstone that cancels rememberKey. Same locking rule.
func (c *Cache) forgetKey(k FileKey) { c.appendKey(k, true) }

func (c *Cache) appendKey(k FileKey, gone bool) {
	c.keys.mu.Lock()
	defer c.keys.mu.Unlock()
	f, err := os.OpenFile(c.keysPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	w := bufio.NewWriter(f)
	err = writeKeyRecord(w, k, gone)
	if err == nil {
		err = w.Flush()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return
	}
	c.keys.lines++
	c.compactKeysIfDueLocked()
}

// compactKeysIfDueLocked rewrites the index from the live keys once the dead
// lines have grown past keysCompactSlack. c.keys.mu must be held; c.mu is
// taken briefly for the snapshot.
func (c *Cache) compactKeysIfDueLocked() {
	c.mu.Lock()
	live := 0
	for _, fs := range c.files {
		if fs.key.RemoteID != "" {
			live++
		}
	}
	if c.keys.lines <= live+keysCompactSlack || c.keys.lines < c.keys.retryAt {
		c.mu.Unlock()
		return
	}
	keys := c.liveKeysLocked()
	c.mu.Unlock()
	c.keys.compactions++
	if err := c.writeKeys(keys); err != nil {
		// Not this append's problem, and not the next one's either: the
		// index is still correct, only longer than it needs to be.
		c.keys.retryAt = c.keys.lines + keysCompactSlack
	}
}

// liveKeysLocked lists every identity the cache still holds a record for, in
// a stable order. c.mu must be held.
func (c *Cache) liveKeysLocked() []FileKey {
	keys := make([]FileKey, 0, len(c.files))
	for _, fs := range c.files {
		if fs.key.RemoteID != "" {
			keys = append(keys, fs.key)
		}
	}
	sortKeys(keys)
	return keys
}

// legacyKeyPath is where a build before the index recorded a file's identity:
// a sidecar in the file's own leaf directory. Reload adopts and removes them.
func (c *Cache) legacyKeyPath(fileHash string) string {
	return filepath.Join(c.fileDir(fileHash), fileHash+".key")
}

// readLegacyKey restores an identity written as a sidecar, checking that the
// sidecar's name is what its contents hash to.
func readLegacyKey(path string) (FileKey, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return FileKey{}, false
	}
	parts := strings.Split(string(b), "\n")
	if len(parts) != 3 || parts[1] == "" {
		return FileKey{}, false
	}
	k := FileKey{Remote: parts[0], RemoteID: parts[1], Version: parts[2]}
	if strings.TrimSuffix(filepath.Base(path), ".key") != k.hash() {
		return FileKey{}, false
	}
	return k, true
}

// retireLegacyKeys removes migrated sidecars. The directories they alone kept
// alive go with the other empty leaves, in pruneEmptyLeaves.
func retireLegacyKeys(paths []string) {
	for _, p := range paths {
		_ = os.Remove(p)
	}
}

// pruneEmptyLeaves removes the leaf directories the walk found nothing but
// sidecars in, then each parent that is empty as a result. A directory that
// still holds something refuses the remove, which is the point; reload runs
// before any admission, so nothing is being created in them meanwhile.
func pruneEmptyLeaves(leaves []string, content map[string]int) {
	parents := map[string]bool{}
	for _, leaf := range leaves {
		if content[leaf] > 0 {
			continue
		}
		if os.Remove(leaf) == nil {
			parents[filepath.Dir(leaf)] = true
		}
	}
	for dir := range parents {
		_ = os.Remove(dir)
	}
}

// keysError wraps an index failure for New's caller.
func keysError(op string, err error) error {
	return fmt.Errorf("cache: reload: %s key index: %w", op, err)
}
