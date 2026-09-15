package mcpsrv

import (
	"context"
	"testing"
	"time"

	"cloudfs/internal/agent"
	"cloudfs/internal/config"
	"cloudfs/internal/index"
)

// lateIndex is an IndexService bound after the server is built. The
// indexer needs the FS that newEnv creates, while New needs a non-nil
// Index to register the tools, so the tests hand New this shell first and
// fill it once the FS exists. Nothing calls it before it is bound.
type lateIndex struct{ x *index.Indexer }

func (l *lateIndex) Search(ctx context.Context, q index.SearchQuery) (index.SearchResult, error) {
	return l.x.Search(ctx, q)
}

func (l *lateIndex) Status(ctx context.Context, p string) (index.Status, error) {
	return l.x.Status(ctx, p)
}

func (l *lateIndex) AddRule(ctx context.Context, r index.Rule) error { return l.x.AddRule(ctx, r) }

func (l *lateIndex) RemoveRule(ctx context.Context, p string) error { return l.x.RemoveRule(ctx, p) }

func (l *lateIndex) Text(ctx context.Context, p string, off int64, max int) (index.TextPage, error) {
	return l.x.Text(ctx, p, off, max)
}

// bindIndex builds an indexer over e's FS with cfg and binds it to the
// shell the server was given. The worker is not started: the tests call
// ReconcileNow so that extraction happens on their own goroutine.
func bindIndex(t *testing.T, e *env, shell *lateIndex, cfg config.Index) *index.Indexer {
	t.Helper()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	st, err := index.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	x, err := index.New(index.Options{FS: e.fs, Store: st, Config: cfg, StartDelay: time.Hour, ReconcileEvery: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { x.Close() })
	shell.x = x
	return x
}

// newIndexEnv is newEnv with an indexer behind the index tools.
func newIndexEnv(t *testing.T, opt Options, cfg config.Index) (*env, *index.Indexer) {
	t.Helper()
	shell := &lateIndex{}
	opt.Index = shell
	e := newEnv(t, opt)
	return e, bindIndex(t, e, shell, cfg)
}

// newIndexAgentEnv is newAgentEnv with an indexer behind the index tools.
func newIndexAgentEnv(t *testing.T, opt Options, sc agent.Scope, cfg config.Index) (*env, *agent.Store, *index.Indexer) {
	t.Helper()
	shell := &lateIndex{}
	opt.Index = shell
	e, st := newAgentEnv(t, opt, sc)
	return e, st, bindIndex(t, e, shell, cfg)
}

// reconcile runs one synchronous pass so the tests see the index settle
// without a worker.
func reconcile(t *testing.T, x *index.Indexer) {
	t.Helper()
	if _, err := x.ReconcileNow(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// listDirs warms directory listings so meta knows the seeded tree the way
// a readdir or the crawler would have left it.
func (e *env) listDirs(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if _, err := e.fs.ReadDirPath(context.Background(), p); err != nil {
			t.Fatalf("list %s: %v", p, err)
		}
	}
}
