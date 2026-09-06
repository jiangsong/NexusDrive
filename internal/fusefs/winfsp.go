//go:build windows && winfsp

// On a `-tags winfsp` Windows build, fusefs is a thin façade over
// internal/winfs: the rest of the tree keeps calling fusefs.MountFS and friends
// unchanged, while the real WinFsp adapter lives in its own package. The type
// aliases make fusefs.Mount and winfs.Mount the same type, so cmd/cloudfs
// compiles against either backend without a build tag of its own.
package fusefs

import "cloudfs/internal/winfs"

type (
	Options      = winfs.Options
	MountOptions = winfs.MountOptions
	OpStats      = winfs.OpStats
	Mount        = winfs.Mount
)

var (
	MountFS              = winfs.MountFS
	ServeBackground      = winfs.ServeBackground
	Supported            = winfs.Supported
	Platform             = winfs.Platform
	VerifyMountable      = winfs.VerifyMountable
	PassthroughEnabled   = winfs.PassthroughEnabled
	PassthroughAvailable = winfs.PassthroughAvailable
)
