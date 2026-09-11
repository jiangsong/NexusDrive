package daemon

import (
	"os"
	"testing"
)

// TestReadaheadRequestEnv checks CLOUDFS_READAHEAD_REQUEST parsing: unset
// keeps the vfs-derived default (0), a plain number is bytes, a size string
// is converted, and a malformed value is rejected rather than silently
// falling back.
func TestReadaheadRequestEnv(t *testing.T) {
	const key = "CLOUDFS_READAHEAD_REQUEST"
	t.Cleanup(func() { os.Unsetenv(key) })

	os.Unsetenv(key)
	got, err := readaheadRequest()
	if err != nil || got != 0 {
		t.Fatalf("unset env: got (%d, %v), want (0, nil)", got, err)
	}

	os.Setenv(key, "262144")
	got, err = readaheadRequest()
	if err != nil || got != 262144 {
		t.Fatalf("numeric env: got (%d, %v), want (262144, nil)", got, err)
	}

	os.Setenv(key, "16MiB")
	got, err = readaheadRequest()
	if err != nil || got != 16<<20 {
		t.Fatalf("size-string env: got (%d, %v), want (%d, nil)", got, err, 16<<20)
	}

	os.Setenv(key, "not-a-size")
	if _, err := readaheadRequest(); err == nil {
		t.Fatal("malformed env: got nil error, want a parse error")
	}
}
