package mcpsrv

import (
	"context"
	"fmt"
	"testing"
	"time"

	"cloudfs/internal/vfs"
)

// waitFor polls until cond holds, so these tests do not depend on the 25 ms
// coalescing tick landing at any particular moment.
// idleToken stands in for a live subscription: run and stop only need a token
// they can cancel.
func idleToken() *subscriptionToken {
	return &subscriptionToken{cancel: func() {}}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestAStalledSubscriberDoesNotStopChangeDetection: the SDK broadcasts a
// resource update to its subscribers one after another under a deadline of its
// own, so a client whose transport has stalled holds that call. Sending inline
// from the coalescing loop meant this server stopped noticing changes at all
// while that lasted — one stuck reader freezing every other client's view.
//
// Delivery therefore happens on its own goroutine behind a bounded queue. This
// fills that queue and checks that changes are still observed and still marked
// for delivery, which is what "the stall is that client's problem" means here.
func TestAStalledSubscriberDoesNotStopChangeDetection(t *testing.T) {
	r := &resourceSubscriptions{}
	r.init(&Server{})
	watched := make([]*resourceWatch, 0, deliveryQueue+8)
	uris := map[string]*resourceWatch{}
	for i := 0; i < deliveryQueue+8; i++ {
		w := &resourceWatch{path: fmt.Sprintf("/work/file-%d.txt", i), ready: true, token: idleToken()}
		uris[fmt.Sprintf("cloudfs://demo/work/file-%d.txt", i)] = w
		watched = append(watched, w)
	}
	r.sessions[nil] = uris

	changes := make(chan vfs.Change)
	r.wg.Add(1)
	go r.run(changes)
	// No deliver goroutine: this is the stalled client, permanently.
	defer func() {
		r.stop()
		r.wg.Wait()
	}()

	// Everything the session watches changes at once.
	select {
	case changes <- vfs.Change{Rescan: true}:
	case <-time.After(5 * time.Second):
		t.Fatal("the coalescing loop did not accept a change")
	}
	waitFor(t, "the delivery queue to fill", func() bool { return len(r.deliveries) == deliveryQueue })

	// The queue is full and nothing is draining it. A further change must
	// still be accepted and still be recorded against its watch: the loop
	// cannot be waiting on the client.
	late := &resourceWatch{path: "/work/late.txt", ready: true, token: idleToken()}
	r.mu.Lock()
	r.sessions[nil]["cloudfs://demo/work/late.txt"] = late
	r.mu.Unlock()
	select {
	case changes <- vfs.Change{Paths: []string{"/work/late.txt"}}:
	case <-time.After(5 * time.Second):
		t.Fatal("a change was refused while the delivery queue was full; the loop is blocked on a client")
	}
	waitFor(t, "the late change to be noticed", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return late.dirty
	})

	// Nothing was dropped either: the watches that could not be queued are
	// still dirty, so a later tick offers them again.
	r.mu.Lock()
	stillDirty := 0
	for _, w := range watched {
		if w.dirty {
			stillDirty++
		}
	}
	queued := len(r.queued)
	r.mu.Unlock()
	if queued != deliveryQueue {
		t.Fatalf("%d URIs are queued, want the queue's %d", queued, deliveryQueue)
	}
	if stillDirty != len(watched) {
		t.Fatalf("%d of %d watches are still marked for delivery; overflow must not drop a notification",
			stillDirty, len(watched))
	}
}

// TestTheDeliveryQueueDoesNotAccumulateDuplicates: while one send is stuck,
// every tick would otherwise offer the same URI again and fill the queue with
// copies of one notification, pushing out everybody else's.
func TestTheDeliveryQueueDoesNotAccumulateDuplicates(t *testing.T) {
	r := &resourceSubscriptions{}
	r.init(&Server{})
	w := &resourceWatch{path: "/work/hot.txt", ready: true, token: idleToken()}
	r.sessions[nil] = map[string]*resourceWatch{"cloudfs://demo/work/hot.txt": w}

	changes := make(chan vfs.Change)
	r.wg.Add(1)
	go r.run(changes)
	defer func() {
		r.stop()
		r.wg.Wait()
	}()

	for i := 0; i < 5; i++ {
		select {
		case changes <- vfs.Change{Paths: []string{"/work/hot.txt"}}:
		case <-time.After(5 * time.Second):
			t.Fatal("the coalescing loop stopped accepting changes")
		}
	}
	waitFor(t, "the URI to be queued", func() bool { return len(r.deliveries) == 1 })
	// Several ticks pass with the sender still stuck.
	time.Sleep(150 * time.Millisecond)
	if n := len(r.deliveries); n != 1 {
		t.Fatalf("the queue holds %d entries for one URI; repeated ticks must not stack duplicates", n)
	}
}

// TestRepeatedSessionChurnReleasesEverythingThisServerHolds: an agent host
// reconnects, and a long-lived daemon sees that many times over. Anything this
// server keys by session or by URI has to come back to nothing each time, or
// the leak is only visible after a week of uptime.
func TestRepeatedSessionChurnReleasesEverythingThisServerHolds(t *testing.T) {
	e := newEnv(t, Options{Allow: []string{"/work"}})
	ctx := context.Background()
	for i := 0; i < 8; i++ {
		e.fake.Seed(fmt.Sprintf("work/file-%d", i), []byte("seed"))
	}
	if _, err := e.fs.ReadDirPath(ctx, "/work"); err != nil {
		t.Fatal(err)
	}

	for round := 0; round < 12; round++ {
		client := newSubscriptionClient(t, e.server)
		for i := 0; i < 8; i++ {
			client.subscribe(t, fmt.Sprintf("cloudfs://ali/work/file-%d", i))
		}
		waitSubscriptionCount(t, e.server, 8)
		if _, err := e.fs.WriteFile(ctx, fmt.Sprintf("/work/file-%d", round%8), []byte("changed"), false); err != nil {
			t.Fatal(err)
		}
		client.session.Close()
		waitSubscriptionCount(t, e.server, 0)
	}

	// The per-session bookkeeping is released by the goroutine waiting on the
	// session, which can finish just after the count reaches zero.
	r := &e.server.subscriptions
	waitFor(t, "every per-session structure to be released", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return len(r.sessions) == 0 && len(r.streams) == 0 && r.count == 0 &&
			len(r.queued) == 0 && len(r.deliveries) == 0
	})
}
