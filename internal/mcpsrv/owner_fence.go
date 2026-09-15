package mcpsrv

import (
	"context"
	"fmt"
)

// errNonOwnerWrite is what every mutating tool returns on a NonOwner server.
// A stdio server started while `cloudfs mount` owns the cache shares that
// process's meta, block cache directory and journal but runs no uploader
// and cannot publish: a write that reached the VFS would leave a node in
// shared meta and a journal row nobody publishes (TODO.md T-43). So the
// refusal comes first, before the tool touches anything, and names the
// transport that reaches the owner.
var errNonOwnerWrite = fmt.Errorf("this server does not own the cache (is \"cloudfs mount\" running?); changing files, pins, jobs, index rules or sessions %w", errRequiresOwner)

// requireOwner is the write fence of a NonOwner server. Every mutating
// tool calls it after its scope checks and before its first VFS, export or
// index call; TestNonOwnerRefusesEveryMutatingToolBeforeTouchingTheFS
// walks tools/list to keep that true. The refusal is noted for the audit
// row like a scope denial: the call was refused by policy, not by a
// failure of the tool.
func (s *Server) requireOwner(ctx context.Context) error {
	if !s.opt.NonOwner {
		return nil
	}
	recordCheck(ctx, "", errNonOwnerWrite)
	return errNonOwnerWrite
}
