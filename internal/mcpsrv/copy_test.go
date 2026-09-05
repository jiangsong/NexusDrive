package mcpsrv

import (
	"context"
	"strings"
	"testing"
)

func TestCopyToolJournalsAndEnforcesBothPaths(t *testing.T) {
	e := newEnv(t, Options{Allow: []string{"/work"}})
	e.fake.Seed("work/source", []byte("copy through MCP"))
	e.fake.Seed("secret/source", []byte("secret"))
	for _, q := range []moveInput{
		{From: "/secret/source", To: "/work/leak"},
		{From: "/work/source", To: "/secret/leak"},
		{From: "/work/../secret/source", To: "/work/leak"},
	} {
		before := e.fake.TotalCalls()
		res := e.call(t, "copy", q, nil)
		if !res.IsError || !strings.Contains(errText(res), "outside the allowed") {
			t.Fatalf("allowlist: %s", errText(res))
		}
		if e.fake.TotalCalls() != before {
			t.Fatal("denied copy accessed provider")
		}
	}
	q := moveInput{From: "/work/source", To: "/work/dest"}
	if res := e.call(t, "copy", q, nil); res.IsError {
		t.Fatal(errText(res))
	}
	got, err := e.fs.ReadFileRange(context.Background(), q.To, 0, 16)
	if err != nil || string(got) != "copy through MCP" {
		t.Fatalf("read: %q %v", got, err)
	}
	if res := e.call(t, "copy", q, nil); !res.IsError {
		t.Fatal("copy overwrote existing file")
	}
	rows, _, err := e.j.ListActive(context.Background(), "", 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("queue: %v %v", rows, err)
	}
}
