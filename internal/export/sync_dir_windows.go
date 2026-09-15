//go:build windows

package export

// Windows does not expose a directory handle through os.Open that File.Sync
// can flush. The file itself is synced before MoveFileEx-backed Rename; keep
// this hook explicit so Unix does not silently lose its directory fsync.
func syncParent(string) error { return nil }
