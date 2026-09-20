package fusefs

import (
	"context"
	"log"

	"cloudfs/internal/vfs"
)

// applyBirthMode records the permission bits the kernel passed to CREATE or
// MKDIR.
//
// open(2) and mkdir(2) carry the mode of the thing being made, and for some
// callers that is the only place it is ever stated: git checks a script out
// by creating it with the mode from its index, not by calling chmod(2)
// afterwards. The node exists either way by the time this runs, so a mode
// that cannot be stored is logged and left at the default rather than failing
// a create that already succeeded.
//
// A mode the node was born with costs nothing to record, so it is not
// recorded: Chmod is a metadata read plus a write transaction, and under the
// usual umask of 022 open(2) asks for exactly the 0644 and mkdir(2) for
// exactly the 0755 the create already wrote. Only a mode that differs — a
// checked-out script, a private key, a 0700 directory — pays for the second
// write. Mode 0 is such a difference and is stored, not skipped: open(2) with
// a mode of 0 asks for a file nobody may read, and answering 0644 would be a
// lie about what is on disk.
func (r *Root) applyBirthMode(ctx context.Context, ino uint64, mode uint32, at *vfs.Attr) {
	mode &= 0o777
	if !birthModeNeedsStore(mode, at.Mode) {
		return
	}
	if err := r.opt.FS.Chmod(ctx, ino, mode); err != nil {
		log.Printf("fusefs: could not record the mode of a new node: %v", err)
		return
	}
	at.Mode = mode
}

// birthModeNeedsStore reports whether the mode the kernel asked for differs
// from the one the create already wrote, and so whether it is worth a second
// metadata transaction.
func birthModeNeedsStore(want, born uint32) bool {
	return want&0o777 != born&0o777
}
