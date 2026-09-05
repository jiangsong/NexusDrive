package mcpsrv

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestDirectoryToolsAndResourcesDoNotMaterializeLargeCachedListing(t *testing.T) {
	e := newEnv(t, Options{Limits: Limits{MaxEntries: 37, MaxBytes: 4096}})
	for i := range 1200 {
		e.fake.Seed(fmt.Sprintf("dir/file-%04d", i), []byte("x"))
	}
	// A cold refresh builds provider metadata, but must not perform a second
	// full Children read merely to answer the first bounded resource page.
	before := e.fs.Meta().ChildrenScans()
	readResourceTest(t, e, "cloudfs://ali/dir")
	calls := e.fake.TotalCalls()
	var names []string
	cursor := ""
	for {
		var page listOutput
		if result := e.call(t, "list_directory", listInput{Path: "/dir", Cursor: cursor}, &page); result.IsError {
			t.Fatalf("tool page failed: %+v", result)
		}
		if len(page.Entries) > 37 || page.Total != 1200 {
			t.Fatalf("page bound/count: %+v", page)
		}
		for _, entry := range page.Entries {
			names = append(names, entry.Name)
		}
		if !page.Truncated {
			break
		}
		if !strings.HasPrefix(page.NextCursor, "n:") || page.NextCursor == cursor || len(names) > 1200 {
			t.Fatalf("cursor did not advance: %+v", page)
		}
		cursor = page.NextCursor
	}
	if len(names) != 1200 {
		t.Fatalf("tool pagination lost entries: %d", len(names))
	}
	for i, name := range names {
		if name != fmt.Sprintf("file-%04d", i) {
			t.Fatalf("out of order/duplicate: %d %q", i, name)
		}
	}
	names = nil
	uri := "cloudfs://ali/dir"
	for {
		c := readResourceTest(t, e, uri)
		var page directoryResource
		if err := json.Unmarshal([]byte(c.Text), &page); err != nil || len(c.Text) > 4096 || len(page.Entries) > 37 {
			t.Fatalf("resource page: %+v %v", page, err)
		}
		for _, entry := range page.Entries {
			names = append(names, entry.Name)
		}
		if !page.Truncated {
			break
		}
		if page.NextURI == uri || page.NextURI == "" || len(names) > 1200 {
			t.Fatal("resource cursor stalled")
		}
		uri = page.NextURI
	}
	if len(names) != 1200 {
		t.Fatalf("resource pagination lost entries: %d", len(names))
	}
	for i, name := range names {
		if name != fmt.Sprintf("file-%04d", i) {
			t.Fatalf("resource out of order/duplicate: %d %q", i, name)
		}
	}
	if e.fs.Meta().ChildrenScans() != before || e.fake.TotalCalls() != calls {
		t.Fatalf("cached page rescanned/refetched directory: full scans=%d provider calls=%d", e.fs.Meta().ChildrenScans()-before, e.fake.TotalCalls()-calls)
	}
}

func TestDirectoryToolRejectsMalformedCursorsBeforeProviderAccess(t *testing.T) {
	e := newEnv(t, Options{Allow: []string{"/allowed"}})
	for _, cursor := range []string{"junk", "1junk", "1 2", "+1", "01", "-1", " 1", "1\n", "999999999999999999999999", "n:", "n:YQ==", "n:Li4", "n:L2E", "n:_w", strings.Repeat("9", 8193)} {
		if r := e.call(t, "list_directory", listInput{Path: "/allowed", Cursor: cursor}, nil); !r.IsError || !strings.Contains(errText(r), "invalid cursor") {
			t.Fatalf("malformed cursor %q: %+v", cursor, r)
		}
	}
	if r := e.call(t, "list_directory", listInput{Path: "/secret", Cursor: "n:YQ"}, nil); !r.IsError {
		t.Fatal("cursor bypassed allowlist")
	}
	if e.fake.TotalCalls() != 0 {
		t.Fatal("invalid cursor or forbidden path contacted provider")
	}
}

func TestDirectoryToolLegacyCursorAndDeletedNameContinuation(t *testing.T) {
	e := newEnv(t, Options{Limits: Limits{MaxEntries: 2}})
	for _, name := range []string{"a", "b", "c", "d"} {
		e.fake.Seed("dir/"+name, []byte("data"))
	}
	var page listOutput
	e.call(t, "list_directory", listInput{Path: "/dir", Cursor: "2"}, &page)
	if len(page.Entries) != 2 || page.Entries[0].Name != "c" || page.Truncated {
		t.Fatalf("legacy cursor: %+v", page)
	}
	e.call(t, "list_directory", listInput{Path: "/dir", Cursor: "999999"}, &page)
	if len(page.Entries) != 0 || page.Truncated || page.Total != 4 {
		t.Fatalf("legacy cursor beyond end: %+v", page)
	}
	e.call(t, "list_directory", listInput{Path: "/dir"}, &page)
	cursor := page.NextCursor
	if cursor != "n:"+base64.RawURLEncoding.EncodeToString([]byte("b")) {
		t.Fatalf("not a name cursor: %q", cursor)
	}
	for _, name := range []string{"a", "b"} {
		n, err := e.fs.StatPath(context.Background(), "/dir/"+name)
		if err != nil {
			t.Fatal(err)
		}
		if err := e.fs.Meta().Remove(context.Background(), n.Ino); err != nil {
			t.Fatal(err)
		}
	}
	e.call(t, "list_directory", listInput{Path: "/dir", Cursor: cursor}, &page)
	if len(page.Entries) != 2 || page.Entries[0].Name != "c" || page.Entries[1].Name != "d" || page.Truncated || page.Total != 2 {
		t.Fatalf("deleted prefix shifted page: %+v", page)
	}
	// Resource cursors retain the existing wire format and name semantics.
	r, err := e.session.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: "cloudfs://ali/dir?cursor=Yg"})
	if err != nil || !strings.Contains(r.Contents[0].Text, `"name":"c"`) {
		t.Fatalf("resource continuation after deletion: %+v %v", r, err)
	}
}
