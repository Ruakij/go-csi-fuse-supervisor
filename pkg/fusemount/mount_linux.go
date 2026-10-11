//go:build linux

package fusemount

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

func statfsState(path string) (State, error) {
	var st unix.Statfs_t
	err := unix.Statfs(path, &st)
	return classify(uint64(st.Type), err), err
}

func unmount(path string) error {
	if _, err := detachAll(path); err != nil {
		return fmt.Errorf("unmount %s: %w", path, err)
	}
	return nil
}

// maxStacked bounds detachAll, which would otherwise never end if something kept
// mounting at path again.
const maxStacked = 100

// detachAll lazily unmounts everything stacked at path and returns how many
// mounts that was. Nothing mounted there, or no such path, is not an error. The
// path is looked up without walking into the mount, so a dead or hung FUSE mount
// does not hang it.
func detachAll(path string) (int, error) {
	for n := 0; n < maxStacked; n++ {
		// NOFOLLOW: a symlink at path is not a mount point, and its target is
		// none of our business.
		err := unix.Unmount(path, unix.MNT_DETACH|unix.UMOUNT_NOFOLLOW)
		if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOENT) {
			return n, nil
		}
		if err != nil {
			return n, err
		}
		// Another mount may be stacked below the one just detached.
	}
	return maxStacked, fmt.Errorf("more than %d mounts stacked", maxStacked)
}

func bind(source, target string, readOnly bool) error {
	if !readOnly {
		// Recursive, so mounts below source come along: a union keeps its sealed
		// control file as a mount on top of the file.
		if err := unix.Mount(source, target, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
			return fmt.Errorf("bind %s to %s: %w", source, target, err)
		}
		return nil
	}
	// nosuid and nodev: a read-only volume from a remote brings neither setuid
	// files nor device nodes into the container.
	fd, err := cloneTree(source, unix.MountAttr{Attr_set: unix.MOUNT_ATTR_RDONLY | unix.MOUNT_ATTR_NOSUID | unix.MOUNT_ATTR_NODEV})
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if err := unix.MoveMount(fd, "", unix.AT_FDCWD, target, unix.MOVE_MOUNT_F_EMPTY_PATH); err != nil {
		return fmt.Errorf("bind %s to %s: %w", source, target, err)
	}
	return nil
}

// cloneTree returns a detached recursive clone of the tree at source with attr
// applied to every mount in it. The caller closes the descriptor.
func cloneTree(source string, attr unix.MountAttr) (int, error) {
	fd, err := unix.OpenTree(unix.AT_FDCWD, source, unix.OPEN_TREE_CLONE|unix.AT_RECURSIVE|unix.OPEN_TREE_CLOEXEC)
	if err != nil {
		return -1, fmt.Errorf("clone %s: %w", source, err)
	}
	if err := unix.MountSetattr(fd, "", unix.AT_EMPTY_PATH|unix.AT_RECURSIVE, &attr); err != nil {
		unix.Close(fd)
		return -1, fmt.Errorf("set attributes of the clone of %s (needs Linux 5.12): %w", source, err)
	}
	return fd, nil
}
