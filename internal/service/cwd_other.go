//go:build !linux && !darwin

package service

// MountCWDHolders is unavailable on platforms without the Unix lsof cwd
// query. Restart keeps its existing behaviour there.
func MountCWDHolders(string) ([]CWDHolder, error) { return nil, nil }
