//go:build windows

package vfs

// syncDisks is a no-op on Windows. It exists for one benchmark helper
// (DropCaches), where a system-wide flush would need an open handle to each
// volume via FlushFileBuffers — not worth it for a measurement-only path, and
// the cache is emptied regardless.
func syncDisks() {}
