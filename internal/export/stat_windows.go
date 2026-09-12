package export

import "os"

// deviceOf has no stable device number on Windows; a job there relies on the
// write errors themselves to notice that a volume went away.
func deviceOf(os.FileInfo) (uint64, bool) { return 0, false }
