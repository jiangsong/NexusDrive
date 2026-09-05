package mcpsrv

import (
	"context"
	"fmt"
	"testing"
)

func TestSearchAuthorizedResultsAreLimitedAfterScope(t *testing.T) {
	for _, allow := range [][]string{nil, {"/visible"}} {
		t.Run(fmt.Sprint(allow), func(t *testing.T) {
			e := newEnv(t, Options{Allow: allow})
			for i := 0; i < 100; i++ {
				e.fake.Seed(fmt.Sprintf("hidden-%03d.go", i), []byte("hidden"))
			}
			e.fake.Seed("visible/result.go", []byte("visible"))
			if _, err := e.fs.Warm(context.Background(), "/", -1); err != nil {
				t.Fatal(err)
			}
			for _, root := range []string{"", "/visible"} {
				if root == "" && allow == nil {
					continue
				}
				var out searchOutput
				res := e.call(t, "search", searchInput{Path: root, Query: ".go", MaxResults: 1}, &out)
				if res.IsError || len(out.Hits) != 1 || out.Hits[0].Path != "/visible/result.go" || out.Truncated {
					t.Fatalf("scope=%q: %+v %+v", root, res, out)
				}
			}
		})
	}
}
