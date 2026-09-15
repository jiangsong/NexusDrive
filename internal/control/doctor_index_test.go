package control

import (
	"context"
	"strings"
	"testing"

	"cloudfs/internal/i18n"
	"cloudfs/internal/index"
)

func checkByName(checks []Check, name string) (Check, bool) {
	for _, c := range checks {
		if c.Name == name {
			return c, true
		}
	}
	return Check{}, false
}

// TestDoctorReportsIndexHealth: the diagnostics page lists the index the
// way it lists the metadata database, and the two conditions a person can
// act on — failed documents, a text budget about to run out — are warnings
// with the action named.
func TestDoctorReportsIndexHealth(t *testing.T) {
	fi := newFakeIndex()
	fi.status.Docs.Failed = 2
	fi.status.TextBytes, fi.status.MaxTotalText = 95, 100
	d := &Doctor{Index: fi}
	checks := d.Run(context.Background())

	db, ok := checkByName(checks, "index_db")
	if !ok || db.Level != LevelOK || !strings.Contains(db.Detail, "3 documents") || !strings.Contains(db.Detail, "12 chunks") {
		t.Fatalf("index_db: %+v", db)
	}
	failed, ok := checkByName(checks, "index_failed")
	if !ok || failed.Level != LevelWarn || !failed.Fixable || !strings.Contains(failed.Detail, "2 documents") || !strings.Contains(failed.Fix, "doctor --fix") {
		t.Fatalf("index_failed: %+v", failed)
	}
	if zh := failed.Localize(i18n.ZH); !strings.Contains(zh.Detail, "2 个文档") {
		t.Fatalf("index_failed did not localize: %+v", zh)
	}
	budget, ok := checkByName(checks, "index_text_budget")
	if !ok || budget.Level != LevelWarn || !strings.Contains(budget.Detail, "95 B") {
		t.Fatalf("index_text_budget: %+v", budget)
	}
	if _, ok := checkByName(checks, "index_identity"); ok {
		t.Fatal("identity was checked without a metadata store to compare with")
	}

	done := d.Fix(context.Background(), i18n.EN)
	if len(fi.retried) != 1 || fi.retried[0] != "" {
		t.Fatalf("fix did not requeue every failed document: %v", fi.retried)
	}
	if len(done) != 1 || !strings.Contains(done[0], "requeued 2") {
		t.Fatalf("fix report: %v", done)
	}

	// A healthy index raises neither warning, and a daemon without an
	// index says nothing at all.
	healthy := newFakeIndex()
	checks = (&Doctor{Index: healthy}).Run(context.Background())
	for _, name := range []string{"index_failed", "index_text_budget"} {
		if _, ok := checkByName(checks, name); ok {
			t.Fatalf("%s reported on a healthy index", name)
		}
	}
	if _, ok := checkByName((&Doctor{}).Run(context.Background()), "index_db"); ok {
		t.Fatal("index_db reported without an index")
	}
}

// TestDoctorComparesIndexIdentityWithMeta: an index built against another
// metadata store carries inode numbers that mean nothing here.
func TestDoctorComparesIndexIdentityWithMeta(t *testing.T) {
	f := newFixture(t)
	want, err := f.meta.Identity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	fi := newFakeIndex()
	fi.identity = want
	checks := (&Doctor{Meta: f.meta, Index: fi}).Run(context.Background())
	if c, ok := checkByName(checks, "index_identity"); !ok || c.Level != LevelOK {
		t.Fatalf("matching identity: %+v", c)
	}
	fi.identity = "someone-else"
	checks = (&Doctor{Meta: f.meta, Index: fi}).Run(context.Background())
	if c, ok := checkByName(checks, "index_identity"); !ok || c.Level != LevelWarn || !strings.Contains(c.Fix, "restart") {
		t.Fatalf("mismatched identity: %+v", c)
	}
	fi.identity = ""
	if _, ok := checkByName((&Doctor{Meta: f.meta, Index: fi}).Run(context.Background()), "index_identity"); ok {
		t.Fatal("an index never bound to a store was compared")
	}

	// A failing index is one failed check with the fix named, not a panic
	// in the identity comparison.
	broken := &failingIndex{fakeIndex: fi}
	checks = (&Doctor{Meta: f.meta, Index: broken}).Run(context.Background())
	if c, ok := checkByName(checks, "index_db"); !ok || c.Level != LevelFail || !strings.Contains(c.Detail, "deadline") || c.Fix == "" {
		t.Fatalf("broken index: %+v", c)
	}
	if _, ok := checkByName(checks, "index_identity"); ok {
		t.Fatal("identity was checked on an index that does not answer")
	}
}

type failingIndex struct{ *fakeIndex }

func (f *failingIndex) Status(context.Context, string) (index.Status, error) {
	return index.Status{}, context.DeadlineExceeded
}
