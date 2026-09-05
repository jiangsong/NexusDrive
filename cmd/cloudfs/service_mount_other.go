//go:build !linux && !darwin

package main

func currentUID() int                        { return 0 }
func mountpointMounted(string) (bool, error) { return false, nil }
