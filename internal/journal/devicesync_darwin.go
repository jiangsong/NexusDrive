//go:build darwin

package journal

import (
	"os"
	"syscall"
)

// separateDeviceFlush reports whether this platform distinguishes "hand the
// bytes to the device" from "make the device empty its write cache".
//
// macOS does, and the gap is enormous: fsync(2) returns once the data is in
// the drive's cache, measured at 74 us on this project's development machine
// (APFS on internal NVMe), while F_FULLFSYNC waits for the drive to commit it
// and costs 4.07 ms — fifty-five times more. os.File.Sync is F_FULLFSYNC on
// darwin, which is why every sync on the write path used to be the expensive
// kind whether or not it needed to be.
const separateDeviceFlush = true

// issueWrites hands a file's dirty pages to the device without waiting for
// the device to flush its own cache. Under DurabilityBarrier this is what the
// staging file gets: the bytes are on their way, and the single F_FULLFSYNC
// that follows the row insert is what makes them — and everything else issued
// before it — durable.
func issueWrites(f *os.File) error {
	return syscall.Fsync(int(f.Fd()))
}

// flushDevice asks the drive to commit everything it has been handed.
func flushDevice(f *os.File) error {
	return f.Sync() // os.File.Sync is fcntl(F_FULLFSYNC) on darwin.
}
