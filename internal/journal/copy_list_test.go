package journal

import (
	"context"
	"fmt"
	"testing"
)

func TestCopyListingPagesWithoutAcquiringPreparation(t *testing.T) {
	j, _, dir := openTest(t)
	ctx := context.Background()
	want := map[string]bool{}
	for i := range 5 {
		spec := copySpec(3)
		spec.TargetPath = fmt.Sprintf("/target/%d", i)
		c, err := j.BeginCopy(ctx, spec, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		want[c.Job().ID] = true
		if i == 0 {
			c.Close()
			if err := j.FailCopy(ctx, c.Job().ID, "test failure"); err != nil {
				t.Fatal(err)
			}
		}
	}
	ro, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	cursor := ""
	for {
		jobs, next, err := ro.ListCopyJobs(ctx, cursor, 2)
		if err != nil || len(jobs) == 0 || len(jobs) > 2 {
			t.Fatalf("page: %+v %q %v", jobs, next, err)
		}
		for _, job := range jobs {
			if !want[job.ID] || job.ID <= cursor {
				t.Fatalf("duplicate/out-of-order job: %+v", job)
			}
			delete(want, job.ID)
			cursor = job.ID
		}
		if next == "" {
			break
		}
		if next != cursor {
			t.Fatal("cursor does not end at last returned job")
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing jobs: %+v", want)
	}
	if jobs, next, err := ro.ListCopyJobs(ctx, cursor, 2); err != nil || len(jobs) != 0 || next != "" {
		t.Fatalf("end page: %+v %q %v", jobs, next, err)
	}
	for _, limit := range []int{0, -1, 1001} {
		if _, _, err := ro.ListCopyJobs(ctx, "", limit); err == nil {
			t.Fatalf("accepted limit %d", limit)
		}
	}
	if _, _, err := ro.ListCopyJobs(ctx, "../bad", 1); err == nil {
		t.Fatal("accepted invalid cursor")
	}
}
