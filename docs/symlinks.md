# Symbolic links on mounted drives

CloudFS's FUSE mount supports `symlink(2)`, `readlink(2)` and `lstat(2)`.
Applications can create relative links such as npm's `node_modules/.bin/vite`
and workspace directory links directly in the mount. No separate build
directory or synchronization command is required.

## Storage format

A link named `vite` with target `../vite/bin/vite.js` is stored remotely as
the ordinary file `vite.rclonelink`, containing exactly that target string,
without a newline. This is the rclone link format. The mount exposes `vite`
as a symbolic link and hides its encoded filename. Link contents use the
existing durable write journal, local cache, upload recovery and deletion
queue. Uploaded links can be discovered from a fresh metadata database.

The `.rclonelink` suffix is reserved: creating or renaming an ordinary file,
directory or link to a virtual name ending in that suffix is rejected.
Existing remote regular files with that suffix are interpreted as links.
Do not use that suffix for unrelated data. If the remote contains both
`vite` and `vite.rclonelink`, their virtual names collide; the directory
refresh fails without publishing either partial listing or overwriting
either remote object. A previously complete cached listing may still be
served. Resolve the collision through the provider's own interface.

Links are limited to 4095 target bytes and 244 name bytes (the 11-byte wire
suffix must fit in a 255-byte name). Empty targets and NUL bytes are invalid.
Targets are preserved verbatim: relative paths are relative to the link's
parent; absolute paths are resolved on the accessing machine. Dangling links
are allowed, and the kernel detects link loops. Unlink and rename act on the
link, not its target. Remote web interfaces see the encoded file, not a
clickable filesystem link; older CloudFS clients do not decode it.

## Adapter boundaries

The kernel resolves mounted paths, including directory links and targets
outside the mount, with the caller's ordinary filesystem permissions.
VFS path APIs used by MCP and WebDAV do not follow links: reading the link
returns its target text, writes through its inode fail, and traversal through
a directory link is rejected. This prevents a remotely supplied link from
escaping an adapter's authorized virtual path. VFS Copy preserves a link
instead of copying its target. FUSE directory entries and attributes report
the symbolic-link type; operation counters include `symlink` and `readlink`.

This feature does not implement hard links or a local-only `node_modules`
policy. Dependency files still use ordinary writeback and upload queues;
symbolic-link compatibility does not eliminate small-file upload overhead.

## Verification

```sh
./gow test ./internal/vfs -run TestSymlink -count=1
./gow test ./internal/fusefs -run TestMountSymlink -count=1 -v
```

The mount tests require working FUSE. The npm test additionally requires
Node/npm; it uses a local fixture package and `npm ci --offline`, exercises
package-directory links and executable `.bin` links, runs test/build scripts,
and reinstalls. It makes no registry requests or writes to real cloud accounts.
