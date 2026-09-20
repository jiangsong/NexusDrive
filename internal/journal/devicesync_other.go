//go:build !darwin

package journal

import "os"

// separateDeviceFlush is false everywhere except darwin: elsewhere fsync(2)
// is already defined to leave the data on stable storage, so there is no
// cheaper "issue without flushing" call to reach for. DurabilityBarrier is
// therefore the same cost as DurabilityPower here — it reorders the flush to
// after the row insert, which makes the promise stronger, but it does not
// make it cheaper. The saving is a macOS one.
const separateDeviceFlush = false

// issueWrites is a full fsync here, for the reason above.
func issueWrites(f *os.File) error { return f.Sync() }

// flushDevice is the same call.
func flushDevice(f *os.File) error { return f.Sync() }
