package pool

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"math"
	"time"

	"cloudfs/internal/provider"
)

// A content token is the Version a pool reports for a file. It is a property
// of the content, never of which replica answered: two replicas of the same
// bytes carry the same token, a replica made by repair inherits the token of
// its source, and two machines indexing the same members derive the same
// token from the same observation. The VFS caches bytes and detects
// conflicts by Version, and the refresher treats a change whose Version and
// id match what it knows as a no-op — so a token that flipped whenever a
// different replica answered would invalidate caches and manufacture
// conflicts out of nothing.
//
//	h1:<hex>   from a content hash the member reports (sha1, then md5, sha256)
//	t1:<hex>   from size and mtime (whole seconds), for hashless members
//
// mtimeTolerance is how far apart two hashless replicas' mtimes may be and
// still be taken as one content: backends round timestamps differently.
const mtimeTolerance = 2 * time.Second

// preferredHashes is the order a token picks a hash in.
var preferredHashes = []provider.HashType{provider.HashSHA1, provider.HashMD5, provider.HashSHA256}

// bestHash returns the most preferred content hash an entry reports.
func bestHash(h provider.Hashes) (provider.HashType, string) {
	for _, ht := range preferredHashes {
		if v := h[ht]; v != "" {
			return ht, v
		}
	}
	return "", ""
}

func hashToken(ht provider.HashType, v string) string { return "h1:" + string(ht) + ":" + v }

func timeToken(size int64, mtime time.Time) string {
	sum := sha1.Sum([]byte(fmt.Sprintf("%d|%d", size, mtime.Unix())))
	return "t1:" + hex.EncodeToString(sum[:8])
}

// sameContent reports whether two replica observations describe one content.
// A shared hash decides when both have one; otherwise size must match and
// the mtimes must be within tolerance.
func sameContent(a, b provider.Entry) bool {
	if a.Size != b.Size {
		return false
	}
	for _, ht := range preferredHashes {
		av, bv := a.Hashes[ht], b.Hashes[ht]
		if av != "" && bv != "" {
			return av == bv
		}
	}
	d := a.ModTime.Sub(b.ModTime)
	if d < 0 {
		d = -d
	}
	return d <= mtimeTolerance
}

// tokenFor computes the token of one content group from the observations in
// it: any member's hash, else the authoritative observation's size and mtime.
func tokenFor(group []provider.Entry, authoritative provider.Entry) string {
	for _, ht := range preferredHashes {
		for _, e := range group {
			if v := e.Hashes[ht]; v != "" {
				return hashToken(ht, v)
			}
		}
	}
	return timeToken(authoritative.Size, authoritative.ModTime)
}

// dirToken is the version of a directory entry; directories have no content.
func dirToken(id string) string { return "dir-" + id }

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

var _ = math.MaxInt64
