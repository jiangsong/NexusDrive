//go:build windows && winfsp

package winfs

import "strings"

// Two facts about the Windows namespace do not hold in the remote namespaces
// cloudfs mounts, and reconciling them is a product decision, not a mechanical
// one. Both choices here are deliberate defaults that need sign-off against the
// acceptance run; both are marked UNVERIFIED.
//
// 1. Case sensitivity. The remotes are POSIX and case-sensitive: "Report.txt"
//    and "report.txt" are two files. NTFS preserves case but compares
//    case-insensitively by default, so those two would collide. This adapter
//    mounts case-sensitive (winfs.go: SetCapCaseInsensitive(false)) so the two
//    names stay distinct. The residual risk is a volume or tool that forces
//    case-insensitive comparison anyway; there, the second name is
//    unreachable. The alternative — folding case at the vfs boundary — was
//    rejected because it hides files that exist on the remote.
//
// 2. Reserved names and illegal characters. Windows forbids the characters
//    <>:"|?*, control characters, a trailing dot or space, and the device
//    names CON, PRN, AUX, NUL, COM1-9, LPT1-9 (with or without an extension).
//    A remote can hold any of these. This adapter does not silently rewrite
//    such a name, because a rewrite that is not perfectly reversible corrupts
//    the round-trip (list, then open by the shown name). Instead an
//    unrepresentable name is omitted from a directory listing and refused on
//    create, and the limitation is documented. The alternative — a reversible
//    percent-style escape presented to Windows and decoded on the way back —
//    is the richer answer and is left for the acceptance phase, where its
//    round-trip can actually be tested.

// representable reports whether name can be presented to Windows without a
// lossy rewrite. See the package note for the decision this encodes.
func representable(name string) bool {
	if name == "" || name == "." || name == ".." {
		return name == "." || name == ".."
	}
	if strings.ContainsAny(name, `<>:"|?*/\`) {
		return false
	}
	for _, r := range name {
		if r < 0x20 {
			return false
		}
	}
	if last := name[len(name)-1]; last == '.' || last == ' ' {
		return false
	}
	return !isReservedDeviceName(name)
}

// isReservedDeviceName reports whether name is a Windows reserved device name,
// with or without an extension (CON, NUL, COM1, LPT9, ...).
func isReservedDeviceName(name string) bool {
	stem := name
	if dot := strings.IndexByte(name, '.'); dot >= 0 {
		stem = name[:dot]
	}
	switch strings.ToUpper(stem) {
	case "CON", "PRN", "AUX", "NUL":
		return true
	}
	up := strings.ToUpper(stem)
	if len(up) == 4 && (strings.HasPrefix(up, "COM") || strings.HasPrefix(up, "LPT")) {
		return up[3] >= '1' && up[3] <= '9'
	}
	return false
}
