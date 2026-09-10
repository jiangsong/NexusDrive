package control

import (
	"os"
	"path/filepath"
)

// makeUnwritable takes write permission off the directory holding the config,
// which is what editConfig needs to write its temporary file and rename it.
func makeUnwritable(path string) error {
	return os.Chmod(filepath.Dir(path), 0o500)
}

func restoreWritable(path string) {
	_ = os.Chmod(filepath.Dir(path), 0o700)
}
