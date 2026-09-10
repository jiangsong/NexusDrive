package control

import (
	"strings"
	"testing"
)

// The maintenance verbs each answer a question a person actually has when
// something is wrong: flush the queue now, put every dead upload back, drop
// the kernel's cached pages, rebuild the pool index. All were routed and
// tested with no way to reach them short of curl.
func TestQueueScreenOffersTheBatchActions(t *testing.T) {
	src := webSource(t, "web/screens/transfers.js")
	for _, want := range []string{
		"'/uploads/flush'",
		"'/uploads/retry'",
		"all: true",
		"t('transfers.flush'",
		"t('transfers.retryall'",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("the queue screen has no %s", want)
		}
	}
}

func TestStorageScreenOffersTheCacheActions(t *testing.T) {
	src := webSource(t, "web/screens/storage.js")
	for _, want := range []string{
		"'/cache/drop'",
		"t('storage.drop'",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("the storage screen has no %s", want)
		}
	}
}

// Rebuilding an index and adopting a pool from a marker file are both
// destructive enough that the daemon demands a confirmation; the page must
// ask for one too rather than send confirm blindly on a single click.
func TestPoolScreenOffersRebuildAndJoin(t *testing.T) {
	src := webSource(t, "web/screens/pool.js")
	for _, want := range []string{
		"'/pool/rebuild'",
		"'/pool/join'",
		"confirmDelete(",
		"t('pool.rebuild'",
		"t('pool.join'",
		"m.pending_restart",
		"pool.member.pending.",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("the pool screen has no %s", want)
		}
	}
}
