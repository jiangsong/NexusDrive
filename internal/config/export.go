package config

import (
	"fmt"
	"time"
)

// Export tunes `cloudfs export`: how many files travel at once, how a single
// large file is split across parallel range requests, and how the runner
// behaves when the destination disk goes away (docs/pool-v2.md §5.6).
//
// Every field has a working default from DefaultExport, which Default()
// installs before the YAML is decoded. An absent key therefore keeps the
// default and an explicit key always wins — including `yield_to_foreground:
// false`, which is why that field is a plain bool rather than a pointer.
type Export struct {
	// Transfers is how many files are in flight at once, across all jobs.
	Transfers int `yaml:"transfers"`
	// Streams is how many concurrent range requests one large file gets.
	// A file smaller than MultiRangeMin always uses one.
	Streams int `yaml:"streams"`
	// RangeSize is the size of one download chunk. It deliberately bypasses
	// the block cache, so it is sized for throughput (QPS × request size)
	// rather than for the cache's block granularity.
	RangeSize Size `yaml:"range_size"`
	// MultiRangeMin is the size from which a file is worth several streams.
	MultiRangeMin Size `yaml:"multi_range_min"`
	// YieldToForeground makes the runner stand aside while the kernel is
	// waiting on a read or a write.
	YieldToForeground bool `yaml:"yield_to_foreground"`
	// DiskProbeInterval is how often a job paused because its destination
	// went away re-checks whether the disk is back.
	DiskProbeInterval time.Duration `yaml:"disk_probe_interval"`
	// JobsParallel is how many export jobs run at once. More than one only
	// makes two jobs fight over the same members.
	JobsParallel int `yaml:"jobs_parallel"`
}

// DefaultExport returns the built-in export settings.
func DefaultExport() Export {
	return Export{
		Transfers:         4,
		Streams:           4,
		RangeSize:         32 << 20,
		MultiRangeMin:     64 << 20,
		YieldToForeground: true,
		DiskProbeInterval: 30 * time.Second,
		JobsParallel:      1,
	}
}

// Bounds a running export has to respect whatever the config says.
const (
	maxExportTransfers = 64
	maxExportStreams   = 32
	// A range is one buffer held per stream, so the cap is about memory:
	// transfers × streams × range_size is the runner's ceiling.
	maxExportRangeSize = Size(1 << 30)
	maxExportJobs      = 8
)

// Validate checks the export block and fills in anything left at zero.
// YieldToForeground is the exception: a bool cannot distinguish an explicit
// false from an absent key, so its default comes from Default() installing
// DefaultExport before the YAML is decoded (the same arrangement as
// Control.UI).
func (e *Export) Validate() error {
	if e.Transfers == 0 {
		e.Transfers = DefaultExport().Transfers
	}
	if e.Streams == 0 {
		e.Streams = DefaultExport().Streams
	}
	if e.RangeSize == 0 {
		e.RangeSize = DefaultExport().RangeSize
	}
	if e.MultiRangeMin == 0 {
		e.MultiRangeMin = DefaultExport().MultiRangeMin
	}
	if e.DiskProbeInterval == 0 {
		e.DiskProbeInterval = DefaultExport().DiskProbeInterval
	}
	if e.JobsParallel == 0 {
		e.JobsParallel = DefaultExport().JobsParallel
	}
	if e.Transfers < 0 || e.Transfers > maxExportTransfers {
		return fmt.Errorf("config: export.transfers must be between 1 and %d, got %d", maxExportTransfers, e.Transfers)
	}
	if e.Streams < 0 || e.Streams > maxExportStreams {
		return fmt.Errorf("config: export.streams must be between 1 and %d, got %d", maxExportStreams, e.Streams)
	}
	if e.RangeSize < 64<<10 || e.RangeSize%(64<<10) != 0 {
		return fmt.Errorf("config: export.range_size must be a positive multiple of 64KiB, got %s", e.RangeSize)
	}
	if e.RangeSize > maxExportRangeSize {
		return fmt.Errorf("config: export.range_size must be at most %s, got %s", maxExportRangeSize, e.RangeSize)
	}
	if e.MultiRangeMin < 0 {
		return fmt.Errorf("config: export.multi_range_min must not be negative, got %s", e.MultiRangeMin)
	}
	if e.DiskProbeInterval < 0 {
		return fmt.Errorf("config: export.disk_probe_interval must not be negative, got %s", e.DiskProbeInterval)
	}
	if e.JobsParallel < 0 || e.JobsParallel > maxExportJobs {
		return fmt.Errorf("config: export.jobs_parallel must be between 1 and %d, got %d", maxExportJobs, e.JobsParallel)
	}
	return nil
}
