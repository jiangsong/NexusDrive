//go:build !windows

package fusefs

import "sync/atomic"

// Op names a FUSE request type this adapter serves. Counting them is how a
// benchmark tells "the kernel answered from its own cache" from "every stat
// crossed into this process": the two look the same in wall time on a warm
// tree and differ by two orders of magnitude in cost.
type Op int

const (
	opLookup Op = iota
	opGetattr
	opSetattr
	opOpendir
	opReaddir
	opDirLookup // readdirplus entry served from the directory listing
	opOpen
	opCreate
	opMkdir
	opUnlink
	opRmdir
	opRename
	opRead
	opWrite
	opFlush
	opFsync
	opRelease
	opGetxattr
	opStatfs
	opCopyFileRange
	nOps
)

var opNames = [nOps]string{
	"lookup", "getattr", "setattr", "opendir", "readdir", "dir_lookup", "open", "create",
	"mkdir", "unlink", "rmdir", "rename", "read", "write", "flush", "fsync", "release",
	"getxattr", "statfs", "copy_file_range",
}

// readBucketEdges are the upper bounds of the READ size histogram, in bytes.
// The kernel decides the size of a READ (its own read-ahead grows a 4 KiB
// pread into 16 KiB or more), and that size is what a cold random read
// costs at the backend.
var readBucketEdges = [...]int64{4 << 10, 16 << 10, 64 << 10, 128 << 10, 1 << 20}

// opCounters is the per-mount tally.
type opCounters struct {
	ops         [nOps]atomic.Int64
	readBytes   atomic.Int64
	readBuckets [len(readBucketEdges) + 1]atomic.Int64
}

func (c *opCounters) count(op Op) { c.ops[op].Add(1) }

func (c *opCounters) countRead(n int) {
	c.readBytes.Add(int64(n))
	for i, edge := range readBucketEdges {
		if int64(n) <= edge {
			c.readBuckets[i].Add(1)
			return
		}
	}
	c.readBuckets[len(readBucketEdges)].Add(1)
}

// OpStats is a snapshot of the request counters.
type OpStats struct {
	// Ops maps request name to the number served since mount.
	Ops map[string]int64
	// ReadBytes is the total requested by READ.
	ReadBytes int64
	// ReadSizes is a cumulative histogram of READ sizes keyed by the
	// Prometheus "le" label ("4096", "16384", …, "+Inf").
	ReadSizes map[string]int64
}

func (c *opCounters) snapshot() OpStats {
	s := OpStats{Ops: make(map[string]int64, nOps), ReadSizes: map[string]int64{}}
	for i := Op(0); i < nOps; i++ {
		s.Ops[opNames[i]] = c.ops[i].Load()
	}
	s.ReadBytes = c.readBytes.Load()
	var cum int64
	for i, edge := range readBucketEdges {
		cum += c.readBuckets[i].Load()
		s.ReadSizes[itoa(edge)] = cum
	}
	cum += c.readBuckets[len(readBucketEdges)].Load()
	s.ReadSizes["+Inf"] = cum
	return s
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}
