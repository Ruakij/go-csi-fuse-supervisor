//go:build linux

package fusemount

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"golang.org/x/sys/unix"
)

func replaceIn(ns *os.File, source, target string, readOnly bool) error {
	if !filepath.IsAbs(source) || !filepath.IsAbs(target) {
		return fmt.Errorf("replace %s with %s: paths must be absolute", target, source)
	}
	// The old mount is detached by path: "/" would detach the root of the
	// namespace, and a ".." could lead out of the directory the caller means.
	if target == "/" || filepath.Clean(target) != target {
		return fmt.Errorf("replace %s with %s: target must be a clean path other than /", target, source)
	}
	if ns == nil {
		return fmt.Errorf("replace %s with %s: no mount namespace", target, source)
	}
	// Private, as the clone would otherwise join the peer group of source and
	// follow what happens to it on the host, in a namespace of another process.
	attr := unix.MountAttr{Propagation: unix.MS_PRIVATE}
	if readOnly {
		attr.Attr_set = unix.MOUNT_ATTR_RDONLY
	}
	fd, err := cloneTree(source, attr)
	if err != nil {
		return err
	}
	defer unix.Close(fd)

	err = inMountNS(ns, func() error {
		// All of a stack goes, or the old mount would show again below the new one.
		detached, err := detachAll(target)
		if err != nil {
			return fmt.Errorf("detach %s: %w", target, err)
		}
		if err := unix.MoveMount(fd, "", unix.AT_FDCWD, target, unix.MOVE_MOUNT_F_EMPTY_PATH); err != nil {
			if detached > 0 {
				return fmt.Errorf("attach the clone at %s, which is left without a mount: %w", target, err)
			}
			return fmt.Errorf("attach the clone at %s: %w", target, err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("replace %s with %s in the mount namespace %s: %w", target, source, ns.Name(), err)
	}
	return nil
}

// inMountNS runs f on a thread of its own in the mount namespace ns refers to.
// It fails without running f if that is the namespace of the caller.
func inMountNS(ns *os.File, f func() error) error {
	return onDisposableThread(func() error {
		// setns into a mount namespace fails for a thread that shares its fs_struct.
		if err := unix.Unshare(unix.CLONE_FS); err != nil {
			return fmt.Errorf("unshare: %w", err)
		}
		fd := int(ns.Fd())
		defer runtime.KeepAlive(ns)
		var other, own unix.Stat_t
		if err := errors.Join(unix.Fstat(fd, &other), unix.Stat("/proc/thread-self/ns/mnt", &own)); err != nil {
			return err
		}
		if other.Ino == own.Ino {
			return errors.New("that is the mount namespace of the caller")
		}
		if err := unix.Setns(fd, unix.CLONE_NEWNS); err != nil {
			return fmt.Errorf("setns: %w", err)
		}
		return f()
	})
}

// onDisposableThread runs f on an OS thread that is discarded afterwards, so f may
// change the namespaces and the fs_struct of its thread.
func onDisposableThread(f func() error) error {
	errc := make(chan error, 1)
	go func() {
		// Never unlocked on this path: the runtime terminates the thread when the
		// goroutine exits instead of running other goroutines on it.
		runtime.LockOSThread()
		if unix.Gettid() == unix.Getpid() {
			// The runtime cannot terminate the main thread, it would keep it parked
			// in the other namespace, and /proc/<pid>/ns of the whole process would
			// show that namespace from then on. While this goroutine holds the main
			// thread, the one started here runs on another thread.
			errc <- onDisposableThread(f)
			runtime.UnlockOSThread()
			return
		}
		errc <- f()
	}()
	return <-errc
}
