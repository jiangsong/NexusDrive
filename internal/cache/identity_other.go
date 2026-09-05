//go:build !linux && !darwin

package cache

import "os"

// The mount implementations currently target Linux and macOS. Other builds
// conservatively charge each imported object rather than infer hard-link identity.
type diskIdentity struct{ info os.FileInfo }

func identity(info os.FileInfo) diskIdentity { return diskIdentity{info} }
