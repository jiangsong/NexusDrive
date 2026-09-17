package mcpsrv

import (
	"context"
	"testing"
	"time"

	"cloudfs/internal/agent"
	"cloudfs/internal/config"
	"cloudfs/internal/index"
	"cloudfs/internal/memory"
	"cloudfs/internal/vfs"
)

// lateFS is a memory.FS bound after the server is built, for the same
// reason lateIndex exists: the store must be handed to New before the FS
// newEnv creates exists.
type lateFS struct{ fs *vfs.FS }

func (l *lateFS) StatPath(ctx context.Context, p string) (vfs.Attr, error) {
	return l.fs.StatPath(ctx, p)
}

func (l *lateFS) ReadFileRange(ctx context.Context, p string, off, length int64) ([]byte, error) {
	return l.fs.ReadFileRange(ctx, p, off, length)
}

func (l *lateFS) WriteFile(ctx context.Context, p string, data []byte, appendMode bool) (vfs.Attr, error) {
	return l.fs.WriteFile(ctx, p, data, appendMode)
}

func (l *lateFS) ReadDirPath(ctx context.Context, p string) ([]vfs.Attr, error) {
	return l.fs.ReadDirPath(ctx, p)
}

func (l *lateFS) ReadDirPagePath(ctx context.Context, p string, opt vfs.DirectoryPageOptions) (vfs.DirectoryPage, error) {
	return l.fs.ReadDirPagePath(ctx, p, opt)
}

func (l *lateFS) Mkdir(ctx context.Context, parent uint64, name string) (vfs.Attr, error) {
	return l.fs.Mkdir(ctx, parent, name)
}

func (l *lateFS) Remove(ctx context.Context, parent uint64, name string, recursive bool) error {
	return l.fs.Remove(ctx, parent, name, recursive)
}

// memEnv is an env with the agent store newAgentEnv opened, for the token
// tests.
type memEnv struct {
	*env
	agents *agent.Store
}

// memoryConfig is the store configuration the memory tool tests use:
// rooted under /work, small limits so the budget tests stay small.
func memoryConfig(root string) config.Memory {
	return config.Memory{Root: root, MaxFactBytes: 4 << 10, MaxAgentBytes: 64 << 10}
}

// newMemoryEnv is newAgentEnv (or newEnv when sc is nil) with a memory
// store behind the memory tools. With an index configuration the store
// also gets a real indexer carrying the built-in memory rule, started so
// that a fresh fact is extracted in the background.
func newMemoryEnv(t *testing.T, opt Options, sc *agent.Scope, cfg config.Memory, idx *config.Index) (*memEnv, *memory.Store, *index.Indexer) {
	t.Helper()
	fsShell := &lateFS{}
	var searcher memory.Searcher
	var shell *lateIndex
	if idx != nil {
		shell = &lateIndex{}
		opt.Index = shell
		searcher = shell
	}
	store := memory.New(memory.Options{FS: fsShell, Searcher: searcher, Config: cfg})
	opt.Memory = store
	e := &memEnv{}
	if sc != nil {
		e.env, e.agents = newAgentEnv(t, opt, *sc)
	} else {
		e.env = newEnv(t, opt)
	}
	fsShell.fs = e.fs
	e.fake.Seed("work/.keep", []byte(""))
	var x *index.Indexer
	if idx != nil {
		if err := idx.Validate(); err != nil {
			t.Fatal(err)
		}
		st, err := index.OpenStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		x, err = index.New(index.Options{
			FS: e.fs, Store: st, Config: *idx, StartDelay: time.Hour, ReconcileEvery: time.Hour,
			Builtin: []index.Rule{memory.IndexRule(cfg.Root)},
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { x.Close() })
		shell.x = x
		x.Start(context.Background())
	}
	return e, store, x
}

func (l *lateFS) Rename(ctx context.Context, oldParent uint64, oldName string, newParent uint64, newName string) error {
	return l.fs.Rename(ctx, oldParent, oldName, newParent, newName)
}

func (l *lateFS) RemoteVersionOf(ctx context.Context, p string) (string, error) {
	return l.fs.RemoteVersionOf(ctx, p)
}
