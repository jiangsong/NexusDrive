package index

import (
	"sync"
	"testing"
	"time"
)

func TestBudgetStopsAtTheLimitAndResetsHourly(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	b := NewBudget(100, func() time.Time { return now })
	if ok, _ := b.Take(60, false); !ok {
		t.Fatal("the first take was refused")
	}
	if ok, resume := b.Take(50, false); ok || !resume.Equal(now.Add(time.Hour)) {
		t.Fatalf("Take over the limit = %v %v, want false and the window end", ok, resume)
	}
	// A refused take charges nothing.
	if used, limit := b.Used(); used != 60 || limit != 100 {
		t.Fatalf("Used() = %d/%d after a refusal, want 60/100", used, limit)
	}
	// What still fits is granted.
	if ok, _ := b.Take(40, false); !ok {
		t.Fatal("a take that exactly fills the window was refused")
	}
	if ok, _ := b.Take(1, false); ok {
		t.Fatal("a take past a full window was granted")
	}
	// Just before the hour turns the window is still closed.
	now = now.Add(time.Hour - time.Second)
	if ok, resume := b.Take(50, false); ok || !resume.Equal(now.Add(time.Second)) {
		t.Fatalf("window reset early: %v %v", ok, resume)
	}
	now = now.Add(time.Second)
	if ok, _ := b.Take(50, false); !ok {
		t.Fatal("window did not reset")
	}
	if used, _ := b.Used(); used != 50 {
		t.Fatalf("Used() = %d after the reset, want 50", used)
	}
}

func TestUnofficialRemotesSpendDouble(t *testing.T) {
	b := NewBudget(100, time.Now)
	if ok, _ := b.Take(40, true); !ok {
		t.Fatal("an unofficial take within half the budget was refused")
	}
	if ok, _ := b.Take(15, true); ok {
		t.Fatal("unofficial remote got the full budget")
	}
	if used, _ := b.Used(); used != 80 {
		t.Fatalf("Used() = %d, want 80 (40 charged double)", used)
	}
	// An official remote shares the same counter at face value.
	if ok, _ := b.Take(20, false); !ok {
		t.Fatal("the remaining 20 bytes were refused to an official remote")
	}
	if ok, _ := b.Take(1, false); ok {
		t.Fatal("a full window granted more")
	}
}

func TestBudgetTable(t *testing.T) {
	type step struct {
		advance    time.Duration
		n          int64
		unofficial bool
		ok         bool
	}
	start := time.Unix(1_800_000_000, 0)
	for _, tc := range []struct {
		name    string
		perHour int64
		steps   []step
	}{
		{"unlimited when zero", 0, []step{{0, 1 << 40, false, true}, {0, 1 << 40, true, true}}},
		{"unlimited when negative", -1, []step{{0, 1 << 40, false, true}}},
		{"exact fit", 10, []step{{0, 10, false, true}, {0, 1, false, false}}},
		{"unofficial exact fit", 10, []step{{0, 5, true, true}, {0, 1, true, false}, {0, 1, false, false}}},
		{"oversized request on an untouched window is granted once", 10,
			[]step{{0, 50, false, true}, {0, 1, false, false}, {time.Hour, 50, false, true}}},
		{"oversized request after any spend waits for the window", 10,
			[]step{{0, 1, false, true}, {0, 50, false, false}, {time.Hour, 50, false, true}}},
		{"the window starts at the first take not at construction", 10,
			[]step{{2 * time.Hour, 10, false, true}, {30 * time.Minute, 1, false, false}, {30 * time.Minute, 1, false, true}}},
		{"a long idle gap opens a fresh window", 10,
			[]step{{0, 10, false, true}, {5 * time.Hour, 10, false, true}}},
		{"zero-byte takes always fit", 10, []step{{0, 10, false, true}, {0, 0, false, true}, {0, 0, true, true}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := start
			b := NewBudget(tc.perHour, func() time.Time { return now })
			for i, s := range tc.steps {
				now = now.Add(s.advance)
				ok, resume := b.Take(s.n, s.unofficial)
				if ok != s.ok {
					t.Fatalf("step %d: Take(%d, %v) = %v, want %v", i, s.n, s.unofficial, ok, s.ok)
				}
				if ok && !resume.IsZero() {
					t.Fatalf("step %d: a granted take reported resumeAt %v", i, resume)
				}
				if !ok && (resume.IsZero() || !resume.After(now) || resume.Sub(now) > time.Hour) {
					t.Fatalf("step %d: refused take reported resumeAt %v for now %v", i, resume, now)
				}
			}
		})
	}
}

func TestBudgetUsedReportsTheLimitAndRollsTheWindow(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	b := NewBudget(100, func() time.Time { return now })
	if used, limit := b.Used(); used != 0 || limit != 100 {
		t.Fatalf("fresh Used() = %d/%d", used, limit)
	}
	b.Take(70, false)
	now = now.Add(2 * time.Hour)
	if used, limit := b.Used(); used != 0 || limit != 100 {
		t.Fatalf("Used() after the window passed = %d/%d, want 0/100", used, limit)
	}
	if _, limit := NewBudget(-5, nil).Used(); limit != 0 {
		t.Fatalf("an unlimited budget reports limit %d, want 0", limit)
	}
}

func TestBudgetIsSafeForConcurrentTakes(t *testing.T) {
	b := NewBudget(1000, nil)
	var wg sync.WaitGroup
	var granted sync.Map
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if ok, _ := b.Take(30, false); ok {
				granted.Store(i, true)
			}
		}(i)
	}
	wg.Wait()
	n := 0
	granted.Range(func(any, any) bool { n++; return true })
	if n != 33 {
		t.Fatalf("%d takes of 30 granted from 1000, want 33", n)
	}
	if used, _ := b.Used(); used != 990 {
		t.Fatalf("Used() = %d, want 990", used)
	}
}
