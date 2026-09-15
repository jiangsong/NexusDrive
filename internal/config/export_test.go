package config

import (
	"strings"
	"testing"
	"time"
)

// TestExportDefaultsSurviveAnAbsentBlock: a config that says nothing about
// exports still gets working settings, and an explicit false is not mistaken
// for "unset" — the trap a plain bool falls into when the defaults are
// applied after decoding instead of before.
func TestExportDefaultsSurviveAnAbsentBlock(t *testing.T) {
	c, err := Parse([]byte("mounts:\n  - path: /tmp/m\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := DefaultExport()
	if c.Export != want {
		t.Fatalf("export defaults are %+v, want %+v", c.Export, want)
	}

	c, err = Parse([]byte("export:\n  yield_to_foreground: false\n  transfers: 8\n  range_size: 8MiB\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Export.YieldToForeground {
		t.Fatal("an explicit yield_to_foreground: false was ignored")
	}
	if c.Export.Transfers != 8 || c.Export.RangeSize != 8<<20 {
		t.Fatalf("explicit keys did not win: %+v", c.Export)
	}
	if c.Export.Streams != want.Streams || c.Export.MemoryBudget != want.MemoryBudget || c.Export.DiskProbeInterval != want.DiskProbeInterval {
		t.Fatalf("keys that were not mentioned lost their defaults: %+v", c.Export)
	}
}

func TestExportValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   Export
		want string
	}{
		{"range size must be a whole number of 64KiB blocks", Export{RangeSize: 100}, "range_size"},
		{"range size is bounded", Export{RangeSize: 4 << 30}, "range_size"},
		{"memory budget covers one range", Export{RangeSize: 64 << 20, MemoryBudget: 32 << 20}, "memory_budget"},
		{"transfers are bounded", Export{Transfers: 1000}, "transfers"},
		{"streams are bounded", Export{Streams: 100}, "streams"},
		{"jobs are bounded", Export{JobsParallel: 99}, "jobs_parallel"},
		{"a negative probe interval is refused", Export{DiskProbeInterval: -time.Second}, "disk_probe_interval"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := tc.in
			err := e.Validate()
			if err == nil {
				t.Fatalf("%+v was accepted", tc.in)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not name %s", err, tc.want)
			}
		})
	}
	// A zero value is not an error: it is "use the defaults". The one
	// exception is yield_to_foreground, whose default lives in Default() —
	// a bool cannot tell "false" from "absent", which is the whole reason
	// the defaults are installed before the YAML is decoded.
	e := Export{YieldToForeground: true}
	if err := e.Validate(); err != nil {
		t.Fatal(err)
	}
	if e != DefaultExport() {
		t.Fatalf("validating a zero export produced %+v", e)
	}
}
