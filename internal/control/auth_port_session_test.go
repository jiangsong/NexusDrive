package control

import "testing"

// The port lock has to name the session that holds it, not the account.
//
// The settle goroutine gives the port back when the flow it ran ends, however
// it ended. Keyed by account name, a goroutine winding down from a session that
// was already cancelled releases the lock a *newer* session for the same
// account is holding: the person retries an authorization, the old flow
// finishes its cleanup a moment later, and the lock is gone while the new
// browser tab is still open. A third account then starts, binds nothing —
// 127.0.0.1:53682 is taken — and fails with "address already in use", which is
// precisely what the process-wide lock exists to prevent.
func TestAStaleAuthorizationDoesNotReleaseThePortALiveOneHolds(t *testing.T) {
	r := newAuthRegistry()
	if _, _, ok := r.claim("s1", "first", &authSession{remote: "first"}); !ok {
		t.Fatal("the first authorization could not take the port")
	}
	r.remove("s1") // cancelled, or polled after a terminal status

	if _, _, ok := r.claim("s2", "first", &authSession{remote: "first"}); !ok {
		t.Fatal("the same account could not start again after cancelling")
	}
	r.releasePort("s1") // the abandoned goroutine finally winds down

	holder, inProgress, ok := r.claim("s3", "second", &authSession{remote: "second"})
	if ok {
		t.Fatal("another account took the callback port while a live authorization still holds it")
	}
	if inProgress {
		t.Fatal("the refusal blames a session for the second account, which has none")
	}
	if holder != "first" {
		t.Fatalf("the refusal names %q as holding the port, want \"first\"", holder)
	}
}

// The ordinary sequence still has to work: whoever holds the port gives it back
// when their own flow settles, and the next account starts.
func TestTheSessionThatHoldsThePortReleasesItWhenItSettles(t *testing.T) {
	r := newAuthRegistry()
	if _, _, ok := r.claim("s1", "first", &authSession{remote: "first"}); !ok {
		t.Fatal("the first authorization could not take the port")
	}
	if holder, _, ok := r.claim("s2", "second", &authSession{remote: "second"}); ok || holder != "first" {
		t.Fatalf("a second account started while the first held the port (holder %q, ok %v)", holder, ok)
	}
	r.releasePort("s1")
	if _, _, ok := r.claim("s2", "second", &authSession{remote: "second"}); !ok {
		t.Fatal("the port was not released when the session that held it settled")
	}
}

// A second start for an account that is already authorizing is a different
// refusal from a busy port, and the page says different things about them.
func TestASecondSessionForTheSameAccountIsRefusedAsInProgress(t *testing.T) {
	r := newAuthRegistry()
	if _, _, ok := r.claim("s1", "first", &authSession{remote: "first"}); !ok {
		t.Fatal("the first authorization could not take the port")
	}
	_, inProgress, ok := r.claim("s2", "first", &authSession{remote: "first"})
	if ok {
		t.Fatal("the same account started a second authorization")
	}
	if !inProgress {
		t.Fatal("the refusal reads as a busy port rather than an authorization already running for this account")
	}
}
