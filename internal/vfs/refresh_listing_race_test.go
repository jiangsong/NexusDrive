package vfs

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestADeltaPollNeverMakesADirectoryLookShort holds the line that matters when
// a delta batch lands while directories are being listed: a listing may fail,
// but it may never come back **short**. A caller that is told "I could not
// read this directory" retries or reports; one handed two files where there
// are three has no way to know, and a media scanner in that position skips the
// film silently.
//
// The poller here runs far more often than production's once a minute, so the
// window is hit rather than waited for. Failures are counted separately and
// tolerated: under a continuous stream of changes to the same directory, a
// reader losing the race and saying so is a correct answer.
func TestADeltaPollNeverMakesADirectoryLookShort(t *testing.T) {
	e := newEnv(t, envOpt{dirTTL: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const dirs, filesPer = 60, 2
	for i := 0; i < dirs; i++ {
		for j := 0; j < filesPer; j++ {
			e.fake.Seed(fmt.Sprintf("lib/dir-%03d/file-%d.bin", i, j), []byte("x"))
		}
	}

	refresher := NewRefresher(e.fs, time.Millisecond)
	refresher.Start(ctx)
	defer refresher.Stop()

	var wrong []string
	var refused int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for round := 0; round < 3; round++ {
				for i := 0; i < dirs; i++ {
					p := fmt.Sprintf("/ali/lib/dir-%03d", i)
					kids, err := e.fs.ReadDirPath(ctx, p)
					if err != nil {
						// Refusing is allowed; answering wrongly is not.
						mu.Lock()
						refused++
						mu.Unlock()
						continue
					}
					if len(kids) != filesPer {
						mu.Lock()
						wrong = append(wrong, fmt.Sprintf("%s: %d entries, want %d", p, len(kids), filesPer))
						mu.Unlock()
					}
				}
			}
		}(worker)
	}
	wg.Wait()
	if len(wrong) > 0 {
		limit := len(wrong)
		if limit > 8 {
			limit = 8
		}
		t.Fatalf("%d listings answered with the wrong contents while the delta poller ran; first: %v",
			len(wrong), wrong[:limit])
	}
	t.Logf("%d listings were refused while a change batch was being applied; none answered wrongly", refused)
}
