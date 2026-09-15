package agent

import (
	"errors"
	"testing"
	"time"
)

func TestScopeCheck(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	work := Scope{Read: []string{"/work"}, Write: []string{"/work/.agent"}}
	for _, tc := range []struct {
		name  string
		s     Scope
		p     string
		write bool
		want  string
		err   error
	}{
		{"empty read is the whole mount", Scope{}, "/gd/x", false, "/gd/x", nil},
		{"child of read", work, "/work/a.md", false, "/work/a.md", nil},
		{"the prefix itself", work, "/work", false, "/work", nil},
		{"sibling with shared prefix", work, "/workshop/a", false, "", ErrDenied},
		{"dotdot escapes are cleaned first", work, "/work/../gd/x", false, "", ErrDenied},
		{"dotdot inside the prefix stays inside", work, "/work/a/../b", false, "/work/b", nil},
		{"trailing slash", work, "/work/", false, "/work", nil},
		{"relative path is rooted", work, "work/a.md", false, "/work/a.md", nil},
		{"empty path is the root", Scope{}, "", false, "/", nil},
		{"empty path outside a prefix", work, "", false, "", ErrDenied},
		{"prefix with trailing slash", Scope{Read: []string{"/work/"}}, "/work/a", false, "/work/a", nil},
		{"write inside write", work, "/work/.agent/s1/out.md", true, "/work/.agent/s1/out.md", nil},
		{"write outside write but inside read", work, "/work/notes.md", true, "", ErrDenied},
		{"nil write means same as read", Scope{Read: []string{"/work"}}, "/work/notes.md", true, "/work/notes.md", nil},
		{"empty write means none", Scope{Read: []string{"/work"}, Write: []string{}}, "/work/n", true, "", ErrDenied},
		{"read-only refuses writes with the old message", Scope{ReadOnly: true}, "/a", true, "", ErrReadOnly},
		{"read-only still reads", Scope{ReadOnly: true}, "/a", false, "/a", nil},
		{"sandbox narrows write", Scope{Read: []string{"/work"}, Sandbox: "/work/.agent/s1"}, "/work/notes.md", true, "", ErrDenied},
		{"sandbox allows its own subtree", Scope{Read: []string{"/work"}, Sandbox: "/work/.agent/s1"}, "/work/.agent/s1/out.md", true, "/work/.agent/s1/out.md", nil},
		{"sandbox keeps read", Scope{Read: []string{"/work"}, Sandbox: "/work/.agent/s1"}, "/work/notes.md", false, "/work/notes.md", nil},
		{"sandbox outside write is nothing", Scope{Read: []string{"/work"}, Write: []string{"/work/.agent"}, Sandbox: "/gd/s1"}, "/gd/s1/x", true, "", ErrDenied},
		{"expired", Scope{ExpiresAt: now.Add(-time.Second)}, "/a", false, "", ErrExpired},
		{"expiry instant is already expired", Scope{ExpiresAt: now}, "/a", false, "", ErrExpired},
		{"not yet expired", Scope{ExpiresAt: now.Add(time.Second)}, "/a", false, "/a", nil},
		{"root read", Scope{Read: []string{"/"}}, "/anything", false, "/anything", nil},
		{"second prefix also counts", Scope{Read: []string{"/gd", "/work"}}, "/work/x", false, "/work/x", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.s.CheckAt(tc.p, tc.write, now)
			if !errors.Is(err, tc.err) || (tc.err == nil && got != tc.want) {
				t.Fatalf("CheckAt(%q, %v) = %q, %v; want %q, %v", tc.p, tc.write, got, err, tc.want, tc.err)
			}
			if tc.err != nil && got != "" {
				t.Fatalf("CheckAt(%q, %v) returned %q alongside %v", tc.p, tc.write, got, err)
			}
		})
	}
}

func TestScopeCheckErrorTextsMatchMCPServer(t *testing.T) {
	// mcpsrv returns these exact strings today; A2 swaps its checks for
	// Scope, so clients that match on the text must not notice.
	if got := ErrDenied.Error(); got != "path is outside the allowed directories" {
		t.Fatal(got)
	}
	if got := ErrReadOnly.Error(); got != "this cloudfs MCP server is read-only" {
		t.Fatal(got)
	}
	if got := ErrExpired.Error(); got != "this cloudfs access has expired" {
		t.Fatal(got)
	}
	// A denial names the cleaned path so the audit row is useful.
	_, err := (Scope{Read: []string{"/work"}}).Check("/gd/../gd/x/", false)
	if err == nil || err.Error() != "path is outside the allowed directories: /gd/x" {
		t.Fatal(err)
	}
}

func TestScopeCheckWriteAllowed(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	if err := (Scope{}).CheckWriteAllowed(now); err != nil {
		t.Fatal(err)
	}
	if err := (Scope{Write: []string{}}).CheckWriteAllowed(now); err != nil {
		t.Fatalf("CheckWriteAllowed does not look at paths: %v", err)
	}
	if err := (Scope{ReadOnly: true}).CheckWriteAllowed(now); !errors.Is(err, ErrReadOnly) {
		t.Fatal(err)
	}
	if err := (Scope{ReadOnly: true, ExpiresAt: now}).CheckWriteAllowed(now); !errors.Is(err, ErrExpired) {
		t.Fatalf("expiry wins over read-only: %v", err)
	}
}

func TestNormaliseAndUnder(t *testing.T) {
	for in, want := range map[string]string{
		"":              "/",
		"/":             "/",
		"a":             "/a",
		"/a/":           "/a",
		"//a//b/":       "/a/b",
		"/a/./b":        "/a/b",
		"/a/../b":       "/b",
		"/../../x":      "/x",
		"a/../..":       "/",
		"/work/../gd/x": "/gd/x",
	} {
		if got := Normalise(in); got != want {
			t.Errorf("Normalise(%q) = %q, want %q", in, got, want)
		}
	}
	for _, tc := range []struct {
		p, prefix string
		want      bool
	}{
		{"/", "/", true},
		{"/anything", "/", true},
		{"/work", "/work", true},
		{"/work/a", "/work", true},
		{"/workshop", "/work", false},
		{"/", "/work", false},
		{"/gd", "/work", false},
	} {
		if got := Under(tc.p, tc.prefix); got != tc.want {
			t.Errorf("Under(%q, %q) = %v, want %v", tc.p, tc.prefix, got, tc.want)
		}
	}
}

func TestScopeEffectivePrefixes(t *testing.T) {
	eq := func(t *testing.T, got, want []string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("got %q, want %q", got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("got %q, want %q", got, want)
			}
		}
	}
	eq(t, (Scope{}).EffectiveRead(), []string{"/"})
	eq(t, (Scope{Read: []string{"/work/", "gd"}}).EffectiveRead(), []string{"/work", "/gd"})
	eq(t, (Scope{Read: []string{"/work"}}).EffectiveWrite(), []string{"/work"})
	eq(t, (Scope{Read: []string{"/work"}, Write: []string{}}).EffectiveWrite(), []string{})
	eq(t, (Scope{Read: []string{"/work"}, ReadOnly: true}).EffectiveWrite(), []string{})
	eq(t, (Scope{Read: []string{"/work"}, Sandbox: "/work/.agent/s1/"}).EffectiveWrite(), []string{"/work/.agent/s1"})
	eq(t, (Scope{Read: []string{"/work"}, Write: []string{"/work/.agent"}, Sandbox: "/gd"}).EffectiveWrite(), []string{})
	eq(t, (Scope{Write: []string{"/gd"}, Sandbox: "/"}).EffectiveWrite(), []string{"/gd"})
	if w := (Scope{ReadOnly: true}).EffectiveWrite(); w == nil {
		t.Fatal("EffectiveWrite must return a non-nil empty slice for read-only, nil would mean \"same as read\"")
	}
}

func TestScopeNarrowNeverWidens(t *testing.T) {
	prefixes := [][]string{nil, {"/"}, {"/work"}, {"/work/"}, {"/work/a"}, {"/workshop"}, {"/gd"}, {"/work/../gd"}, {"/work", "/gd"}, {}}
	var scopes []Scope
	for _, r := range prefixes {
		for _, w := range prefixes {
			for _, ro := range []bool{false, true} {
				scopes = append(scopes, Scope{Read: r, Write: w, ReadOnly: ro}, Scope{Read: r, Write: w, Sandbox: "/work/a"})
			}
		}
	}
	for _, parent := range scopes {
		for _, child := range scopes {
			got := parent.Narrow(child)
			if !parent.Covers(got) {
				t.Fatalf("Narrow widened: parent %+v child %+v got %+v", parent, child, got)
			}
			for _, probe := range []string{"/", "/work", "/work/a/b", "/workshop/x", "/gd/x"} {
				for _, write := range []bool{false, true} {
					if _, err := got.Check(probe, write); err == nil {
						if _, perr := parent.Check(probe, write); perr != nil {
							t.Fatalf("narrowed scope allows %q write=%v that parent %+v denies", probe, write, parent)
						}
					}
				}
			}
		}
	}
}

func TestScopeNarrowKeepsTheIntersection(t *testing.T) {
	parent := Scope{Read: []string{"/work", "/gd"}, Write: []string{"/work/.agent"}}
	child := Scope{Read: []string{"/work/"}, Write: nil}
	got := parent.Narrow(child)
	for _, tc := range []struct {
		p     string
		write bool
		ok    bool
	}{
		{"/work/a", false, true},
		{"/gd/x", false, false},
		{"/work/.agent/s1/out", true, true},
		{"/work/a", true, false},
	} {
		_, err := got.Check(tc.p, tc.write)
		if (err == nil) != tc.ok {
			t.Fatalf("narrowed %+v: Check(%q, %v) = %v, want ok=%v", got, tc.p, tc.write, err, tc.ok)
		}
	}
	if got.ExpiresAt.IsZero() != true {
		t.Fatalf("no expiry on either side must stay open-ended, got %v", got.ExpiresAt)
	}
}

func TestScopeNarrowEmptyIntersectionDeniesEverything(t *testing.T) {
	// {"/work"} and {"/gd"} share nothing. An empty Read would mean "whole
	// mount", so the result must be a scope that refuses every check
	// instead: read-only and already expired.
	got := (Scope{Read: []string{"/work"}}).Narrow(Scope{Read: []string{"/gd"}})
	if got.Read == nil || len(got.Read) != 0 || got.Write == nil || len(got.Write) != 0 {
		t.Fatalf("empty intersection must carry empty non-nil prefixes, got %+v", got)
	}
	if !got.ReadOnly || got.ExpiresAt.IsZero() || !got.ExpiresAt.Before(time.Unix(2, 0)) {
		t.Fatalf("empty intersection must be read-only and already expired, got %+v", got)
	}
	for _, probe := range []string{"/", "/work", "/gd", "/work/a"} {
		for _, write := range []bool{false, true} {
			if _, err := got.Check(probe, write); !errors.Is(err, ErrExpired) {
				t.Fatalf("Check(%q, %v) = %v, want ErrExpired", probe, write, err)
			}
		}
	}
	if err := got.CheckWriteAllowed(time.Unix(1_800_000_000, 0)); !errors.Is(err, ErrExpired) {
		t.Fatal(err)
	}
	// Narrowing an already-empty scope stays empty.
	again := got.Narrow(Scope{})
	if _, err := again.Check("/", false); !errors.Is(err, ErrExpired) {
		t.Fatal(err)
	}
	// An empty write intersection with a live read intersection is just an
	// unwritable scope, not the deny-all one.
	rw := (Scope{Read: []string{"/work"}, Write: []string{"/work/a"}}).Narrow(Scope{Write: []string{"/work/b"}})
	if _, err := rw.Check("/work/x", false); err != nil {
		t.Fatal(err)
	}
	if _, err := rw.Check("/work/a/x", true); !errors.Is(err, ErrDenied) {
		t.Fatal(err)
	}
}

func TestScopeNarrowSandbox(t *testing.T) {
	parent := Scope{Read: []string{"/work"}}
	got := parent.Narrow(Scope{Sandbox: "/work/.agent/s1"})
	if _, err := got.Check("/work/notes.md", false); err != nil {
		t.Fatal(err)
	}
	if _, err := got.Check("/work/notes.md", true); !errors.Is(err, ErrDenied) {
		t.Fatal(err)
	}
	if _, err := got.Check("/work/.agent/s1/out.md", true); err != nil {
		t.Fatal(err)
	}
	// The child's sandbox never lets a write escape the parent.
	got = (Scope{Read: []string{"/work"}, Write: []string{}}).Narrow(Scope{Sandbox: "/work/.agent/s1"})
	if _, err := got.Check("/work/.agent/s1/out.md", true); !errors.Is(err, ErrDenied) {
		t.Fatal(err)
	}
}

func TestNarrowTakesTheEarlierExpiry(t *testing.T) {
	a := time.Unix(100, 0)
	b := time.Unix(200, 0)
	if got := (Scope{ExpiresAt: b}).Narrow(Scope{ExpiresAt: a}); !got.ExpiresAt.Equal(a) {
		t.Fatal(got.ExpiresAt)
	}
	if got := (Scope{ExpiresAt: a}).Narrow(Scope{}); !got.ExpiresAt.Equal(a) {
		t.Fatal(got.ExpiresAt)
	}
	if got := (Scope{}).Narrow(Scope{ExpiresAt: a}); !got.ExpiresAt.Equal(a) {
		t.Fatal(got.ExpiresAt)
	}
	if got := (Scope{}).Narrow(Scope{}); !got.ExpiresAt.IsZero() {
		t.Fatal(got.ExpiresAt)
	}
}

func TestScopeCovers(t *testing.T) {
	work := Scope{Read: []string{"/work"}, Write: []string{"/work/.agent"}}
	for _, tc := range []struct {
		name string
		s, o Scope
		want bool
	}{
		{"everything covers everything", Scope{}, Scope{}, true},
		{"everything covers a subtree", Scope{}, work, true},
		{"a subtree does not cover everything", work, Scope{}, false},
		{"itself", work, work, true},
		{"child read", work, Scope{Read: []string{"/work/a"}, Write: []string{}}, true},
		{"sibling read", work, Scope{Read: []string{"/workshop"}, Write: []string{}}, false},
		{"write wider than parent write", work, Scope{Read: []string{"/work"}}, false},
		{"write inside parent write", work, Scope{Read: []string{"/work"}, Write: []string{"/work/.agent/s1"}}, true},
		{"read-only parent covers a read-only child", Scope{Read: []string{"/work"}, ReadOnly: true}, Scope{Read: []string{"/work"}, ReadOnly: true}, true},
		{"read-only parent does not cover a writer", Scope{Read: []string{"/work"}, ReadOnly: true}, Scope{Read: []string{"/work"}}, false},
		{"sandboxed child is covered", work, Scope{Read: []string{"/work"}, Write: []string{"/work"}, Sandbox: "/work/.agent/s1"}, true},
		{"dotdot and trailing slash are cleaned", work, Scope{Read: []string{"/work/a/../b/"}, Write: []string{}}, true},
		{"expired o is covered by anything", Scope{Read: []string{"/gd"}}, Scope{Read: []string{"/work"}, ExpiresAt: time.Unix(1, 0)}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.s.Covers(tc.o); got != tc.want {
				t.Fatalf("Covers = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestScopeDoesNotMutateItsInputs(t *testing.T) {
	read := []string{"/work/", "/gd/../gd"}
	write := []string{"/work/.agent/"}
	s := Scope{Read: read, Write: write}
	s.EffectiveRead()
	s.EffectiveWrite()
	s.Narrow(Scope{Read: []string{"/work"}})
	s.Covers(s)
	if _, err := s.Check("/work/x", true); err == nil {
		t.Fatal("expected /work/x to be outside the write prefix")
	}
	if read[0] != "/work/" || read[1] != "/gd/../gd" || write[0] != "/work/.agent/" {
		t.Fatalf("inputs were rewritten: %q %q", read, write)
	}
}
