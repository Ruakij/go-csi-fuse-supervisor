// Package fusemount inspects, binds, unmounts and repairs the mounts of FUSE
// daemons that outlive the process that started them.
//
// The mount operations need Linux, root and, to reach the mount namespace of a
// daemon by its pid, a /proc that shows the daemon (hostPID in a pod). On other systems the package builds, Status
// reports Other and the operations return an error, so callers still vet there.
package fusemount

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
)

// State is what a path is, as far as repairing a FUSE mount is concerned.
type State int

const (
	// Other is anything that is not one of the others: a plain directory, a
	// mount of another filesystem, an error that says nothing about FUSE.
	Other State = iota
	// Live is a FUSE mount whose daemon answers.
	Live
	// Dead is a FUSE mount whose daemon is gone: every call fails with ENOTCONN
	// until the mount is detached.
	Dead
	// Gone is a path that does not exist.
	Gone
)

func (s State) String() string {
	switch s {
	case Live:
		return "live"
	case Dead:
		return "dead"
	case Gone:
		return "gone"
	default:
		return "other"
	}
}

// fuseSuperMagic identifies a FUSE mount in statfs.
const fuseSuperMagic = 0x65735546

// classify maps the outcome of statfs to a State.
func classify(fsType uint64, statfsErr error) State {
	switch {
	case statfsErr == nil && fsType == fuseSuperMagic:
		return Live
	case errors.Is(statfsErr, syscall.ENOTCONN):
		return Dead
	case errors.Is(statfsErr, fs.ErrNotExist):
		return Gone
	default:
		return Other
	}
}

// Status reports what path is. It uses statfs, which unlike stat is never
// answered from the kernel's attribute cache, so a daemon that died is noticed
// at once.
//
// Status blocks for as long as the call does: a FUSE mount whose daemon is alive
// but stuck never answers. Use a [Prober] where that must not stall the caller.
func Status(path string) State {
	s, _ := statfsState(path)
	return s
}

// IsFUSE reports whether path is a live FUSE mount.
func IsFUSE(path string) bool { return Status(path) == Live }

// Unmount detaches whatever is mounted at path, lazily, so it also works when the
// daemon is dead or hung or files are still open. Mounts stacked on one another at path
// are all detached.
//
// Unmount is idempotent: a path that is not a mount point, or does not exist, is
// not an error. It needs root, as unmounting does.
func Unmount(path string) error { return unmount(path) }

// Bind makes the tree at source, with the mounts below it, visible at target,
// read-only if asked. The target must exist.
//
// A read-only bind is made read-only, nosuid and nodev before it is attached.
// Remounting a bind afterwards would change it in the mount namespace of the
// caller only, while a copy that propagated to the host, as the target of a CSI
// volume does, stays writable. Needs Linux 5.12.
func Bind(source, target string, readOnly bool) error { return bind(source, target, readOnly) }

// ReplaceIn replaces whatever is mounted at target in the mount namespace ns,
// an open file of it such as /proc/<pid>/ns/mnt, with a clone of the tree at
// source, read-only if asked. The clone is made in the mount namespace of the
// caller, and attached in the other one with everything mounted below source,
// private to it: what the host mounts or unmounts at source afterwards does not
// reach the clone.
//
// This repairs a stale bind inside a sandboxed daemon, such as a branch that
// was replaced on the host while the daemon kept the old, disconnected mount,
// without restarting the daemon and without touching the mount that its
// consumers hold. The old mount at target is detached by path, which does not
// walk into it, so a dead or hung FUSE mount there cannot hang the call. The
// target must exist in the namespace. If attaching the clone fails after that,
// the old mount is already detached and the error says so.
//
// The paths must be absolute, and target clean and not "/". target is resolved
// from the root of the mount namespace, not from the root of a process in it,
// which differs for a chroot. The clone is private, but attaching it and
// detaching the old mount happen in the mount that contains target, and
// propagate to the peers of that mount if it is shared, which can reach the
// host. That mount must be private or a slave, as it is in a sandbox that makes
// its mounts rslave.
//
// ReplaceIn refuses the mount namespace of the caller. It cannot tell whose
// namespace ns is: a caller that opened it through a pid must make sure afterwards
// that the pid still is the process it meant and was not reused by another one
// with the same paths in its namespace, as Supervisor.ReplaceIn of package
// supervisor does for the daemon of a volume. It needs root, Linux 5.12, and a
// /proc in which the process is visible.
func ReplaceIn(ns *os.File, source, target string, readOnly bool) error {
	return replaceIn(ns, source, target, readOnly)
}
