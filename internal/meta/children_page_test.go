package meta

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestChildrenPageOrderingContinuationAndLegacyOffsets(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	names := []string{"a", "B", "space name", "quote\"", "percent%", "中文", "é", "emoji🙂"}
	for _, name := range names {
		if _, err := s.Upsert(ctx, file(RootIno, name, 1)); err != nil {
			t.Fatal(err)
		}
	}
	sort.Strings(names)
	var got []string
	after := ""
	for {
		page, err := s.ChildrenPage(ctx, RootIno, after, 0, 3)
		if err != nil || len(page) > 3 {
			t.Fatalf("page: %+v %v", page, err)
		}
		if len(page) == 0 {
			break
		}
		for _, n := range page {
			got = append(got, n.Name)
		}
		after = page[len(page)-1].Name
	}
	if !reflect.DeepEqual(got, names) {
		t.Fatalf("ordering or pagination mismatch: %q != %q", got, names)
	}
	for _, offset := range []int{0, 2, len(names), 1 << 30} {
		page, err := s.ChildrenPage(ctx, RootIno, "", offset, 2)
		if err != nil {
			t.Fatal(err)
		}
		want := names[min(offset, len(names)):min(offset+2, len(names))]
		if len(page) != len(want) {
			t.Fatalf("offset %d: %v", offset, page)
		}
		for i, n := range page {
			if n.Name != want[i] {
				t.Fatalf("offset %d: %v", offset, page)
			}
		}
	}
	count, err := s.ChildrenCount(ctx, RootIno)
	if err != nil || count != len(names) || s.ChildrenScans() != 0 {
		t.Fatalf("count=%d err=%v scans=%d", count, err, s.ChildrenScans())
	}
	for _, tc := range []struct {
		after         string
		offset, limit int
	}{{"", 0, 0}, {"", 0, 4097}, {"", -1, 2}, {"a", 1, 2}} {
		if _, err := s.ChildrenPage(ctx, RootIno, tc.after, tc.offset, tc.limit); err == nil {
			t.Fatalf("accepted invalid page %+v", tc)
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.ChildrenPage(cancelled, RootIno, "", 0, 1); err == nil {
		t.Fatal("cancelled query succeeded")
	}
}

func TestChildrenPageSeeksIndexInLargeDirectory(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	const count = 10000
	nodes := make([]Node, count)
	for i := range nodes {
		nodes[i] = file(RootIno, fmt.Sprintf("file-%05d", i), 1)
	}
	if err := s.PutDir(ctx, RootIno, nodes, time.Minute, nil); err != nil {
		t.Fatal(err)
	}
	rows, err := s.DB().QueryContext(ctx, "EXPLAIN QUERY PLAN "+childrenPageSQL, RootIno, RootIno, "file-09000", 11, 0)
	if err != nil {
		t.Fatal(err)
	}
	var plans []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plans = append(plans, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	plan := strings.Join(plans, "\n")
	if !strings.Contains(plan, "nodes_parent_name (parent_ino=? AND name>?)") || strings.Contains(plan, "SCAN nodes") || strings.Contains(plan, "TEMP B-TREE") {
		t.Fatalf("page does not seek bounded ordered index: %s", plan)
	}
	page, err := s.ChildrenPage(ctx, RootIno, "file-09000", 0, 11)
	if err != nil || len(page) != 11 || page[0].Name != "file-09001" || page[10].Name != "file-09011" {
		t.Fatalf("large directory page: %+v %v", page, err)
	}
	if s.ChildrenScans() != 0 {
		t.Fatal("page performed a full children read")
	}
}
