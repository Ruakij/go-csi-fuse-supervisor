//go:build linux && mounttest

package fusemount

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// helperEnv switches the test binary into the process that holds a mount
// namespace of its own for ReplaceIn to work in.
const helperEnv = "FUSEMOUNT_TEST_NS_HELPER"

// TestMain runs the namespace helper when re-executed by startNS.
func TestMain(m *testing.M) {
	if mode := os.Getenv(helperEnv); mode != "" {
		if err := runNSHelper(mode); err != nil {
			fmt.Fprintln(os.Stderr, "helper:", err)
			os.Exit(1)
		}
		return
	}
	os.Exit(m.Run())
}

// runNSHelper makes the mounts of the namespace private, binds every path given
// in the arguments to the path named after "=" (the pod's own copy), optionally
// pivots into a fresh root, reports "ready" and waits for stdin to close.
func runNSHelper(mode string) error {
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return err
	}
	for _, arg := range os.Args[1:] {
		src, dst, _ := strings.Cut(arg, "=")
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return err
		}
		if err := unix.Mount(src, dst, "", unix.MS_BIND, ""); err != nil {
			return err
		}
	}
	if mode == "pivot" {
		if err := pivot(); err != nil {
			return fmt.Errorf("pivot: %w", err)
		}
	}
	fmt.Println("ready")
	_, _ = bufio.NewReader(os.Stdin).ReadString(0)
	return nil
}

// pivot moves the process into an empty tmpfs root, the way a runtime does for
// a container, so that nothing of the host tree is visible by path.
func pivot() error {
	root, err := os.MkdirTemp("", "newroot")
	if err != nil {
		return err
	}
	if err := unix.Mount("tmpfs", root, "tmpfs", 0, ""); err != nil {
		return err
	}
	// The pod copy and the target live on the new root as well.
	for _, arg := range os.Args[1:] {
		_, dst, _ := strings.Cut(arg, "=")
		if err := os.MkdirAll(filepath.Join(root, dst), 0o755); err != nil {
			return err
		}
		if err := unix.Mount(dst, filepath.Join(root, dst), "", unix.MS_BIND, ""); err != nil {
			return err
		}
	}
	old := filepath.Join(root, "old")
	if err := os.Mkdir(old, 0o755); err != nil {
		return err
	}
	if err := unix.PivotRoot(root, old); err != nil {
		return err
	}
	if err := os.Chdir("/"); err != nil {
		return err
	}
	return unix.Unmount("/old", unix.MNT_DETACH)
}

// startNS starts a helper in a new mount namespace that is a copy of this one,
// with the given "source=pod copy" binds in it.
func startNS(t *testing.T, mode string, binds ...string) int {
	t.Helper()
	cmd := exec.Command(os.Args[0], binds...)
	cmd.Env = append(os.Environ(), helperEnv+"="+mode)
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWNS}
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the helper (needs CAP_SYS_ADMIN): %v", err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Wait()
	})
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "ready" {
		t.Fatalf("helper did not become ready: %q, %v", line, err)
	}
	return cmd.Process.Pid
}

// hostTmpfs mounts a tmpfs with a file in it, as the source of a replacement.
func hostTmpfs(t *testing.T, dir, file string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mount("tmpfs", dir, "tmpfs", 0, ""); err != nil {
		t.Fatalf("mount tmpfs: %v", err)
	}
	t.Cleanup(func() { _ = Unmount(dir) })
	if err := os.WriteFile(filepath.Join(dir, file), []byte(file), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBind(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "src")
	hostTmpfs(t, src, "f")

	rw := filepath.Join(tmp, "rw")
	ro := filepath.Join(tmp, "ro")
	for _, d := range []string{rw, ro} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := Bind(src, rw, false); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = Unmount(rw) })
	if err := Bind(src, ro, true); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = Unmount(ro) })

	if b, err := os.ReadFile(filepath.Join(rw, "f")); err != nil || string(b) != "f" {
		t.Errorf("read through the rw bind = %q, %v", b, err)
	}
	if err := os.WriteFile(filepath.Join(rw, "g"), nil, 0o644); err != nil {
		t.Errorf("write through the rw bind: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ro, "h"), nil, 0o644); !errors.Is(err, unix.EROFS) {
		t.Errorf("write through the ro bind = %v, want EROFS", err)
	}
	// The source stays writable: read-only belongs to the bind only.
	if err := os.WriteFile(filepath.Join(src, "i"), nil, 0o644); err != nil {
		t.Errorf("write to the source: %v", err)
	}
	// The read-only bind drops setuid files and device nodes, the source keeps them.
	const nosuidNodev = unix.MS_NOSUID | unix.MS_NODEV
	if got := mountFlags(t, ro); got&nosuidNodev != nosuidNodev {
		t.Errorf("flags of the ro bind = %#x, want nosuid and nodev", got)
	}
	if got := mountFlags(t, src); got&nosuidNodev != 0 {
		t.Errorf("flags of the source = %#x, want it untouched", got)
	}
}

// mountFlags returns the flags of the mount at path as statfs reports them.
func mountFlags(t *testing.T, path string) int64 {
	t.Helper()
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		t.Fatal(err)
	}
	return st.Flags
}

func TestUnmount(t *testing.T) {
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "m")
	hostTmpfs(t, dir, "f")

	if err := Unmount(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "f")); !os.IsNotExist(err) {
		t.Errorf("the mount is still there: %v", err)
	}
	if err := Unmount(dir); err != nil {
		t.Errorf("second Unmount: %v", err)
	}
	if err := Unmount(filepath.Join(tmp, "missing")); err != nil {
		t.Errorf("Unmount of a missing path: %v", err)
	}
}

// Unmount removes every mount stacked at the path.
func TestUnmountStacked(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "m")
	hostTmpfs(t, dir, "one")
	if err := unix.Mount("tmpfs", dir, "tmpfs", 0, ""); err != nil {
		t.Fatal(err)
	}
	if err := Unmount(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "one")); !os.IsNotExist(err) {
		t.Errorf("a mount is left below: %v", err)
	}
	if err := unix.Unmount(dir, unix.MNT_DETACH); err == nil {
		t.Error("a mount was left at the path")
	}
}

func TestUnmountDead(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "m")
	srv := newFUSE(t, dir)
	srv.kill()
	if got := Status(dir); got != Dead {
		t.Fatalf("Status = %v, want dead", got)
	}
	if err := Unmount(dir); err != nil {
		t.Fatal(err)
	}
	if got := Status(dir); got == Dead || got == Live {
		t.Errorf("Status after Unmount = %v", got)
	}
}

func inNS(pid int, path string) string { return fmt.Sprintf("/proc/%d/root%s", pid, path) }

// nsOf opens the mount namespace of pid, as a caller of ReplaceIn does.
func nsOf(t *testing.T, pid int) *os.File {
	t.Helper()
	return openNS(t, fmt.Sprintf("/proc/%d/ns/mnt", pid))
}

func openNS(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

// A branch bind in a running process that went stale is replaced in the
// namespace of that process, while the copy another mount holds stays as it is.
func TestReplaceInDeadMount(t *testing.T) {
	for _, mode := range []string{"plain", "pivot"} {
		t.Run(mode, func(t *testing.T) {
			tmp := t.TempDir()
			branch := filepath.Join(tmp, "branch")
			pod := filepath.Join(tmp, "pod")
			srv := newFUSE(t, branch)

			// The helper copies the mount to the pod path (a container bind) before
			// the daemon dies, and keeps the branch path as its own bind.
			pid := startNS(t, mode, branch+"="+branch, branch+"="+pod)
			srv.kill()

			if got := Status(inNS(pid, branch)); got != Dead {
				t.Fatalf("branch in the namespace = %v, want dead", got)
			}

			fresh := filepath.Join(tmp, "fresh")
			hostTmpfs(t, fresh, "new")
			if err := ReplaceIn(nsOf(t, pid), fresh, branch, false); err != nil {
				t.Fatal(err)
			}

			if b, err := os.ReadFile(inNS(pid, filepath.Join(branch, "new"))); err != nil || string(b) != "new" {
				t.Errorf("replacement in the namespace = %q, %v", b, err)
			}
			if got := Status(inNS(pid, pod)); got != Dead {
				t.Errorf("pod copy = %v, want it untouched (dead)", got)
			}
			if got := Status(branch); got != Dead {
				t.Errorf("branch on the host = %v, want it untouched (dead)", got)
			}
			if mode == "plain" {
				if err := os.WriteFile(inNS(pid, filepath.Join(branch, "w")), nil, 0o644); err != nil {
					t.Errorf("write through the rw replacement: %v", err)
				}
			}
		})
	}
}

func TestReplaceInReadOnly(t *testing.T) {
	tmp := t.TempDir()
	branch := filepath.Join(tmp, "branch")
	fresh := filepath.Join(tmp, "fresh")
	hostTmpfs(t, fresh, "new")
	hostTmpfs(t, branch, "old")
	pid := startNS(t, "plain", branch+"="+branch)

	if err := ReplaceIn(nsOf(t, pid), fresh, branch, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(inNS(pid, filepath.Join(branch, "old"))); !os.IsNotExist(err) {
		t.Errorf("the old mount shows through: %v", err)
	}
	if err := os.WriteFile(inNS(pid, filepath.Join(branch, "w")), nil, 0o644); !errors.Is(err, unix.EROFS) {
		t.Errorf("write through the ro replacement = %v, want EROFS", err)
	}
	if err := os.WriteFile(filepath.Join(fresh, "w"), nil, 0o644); err != nil {
		t.Errorf("source on the host: %v", err)
	}
}

// A mount that is not there yet is attached without anything to detach.
func TestReplaceInNothingMounted(t *testing.T) {
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "dir")
	fresh := filepath.Join(tmp, "fresh")
	hostTmpfs(t, fresh, "new")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	pid := startNS(t, "plain")
	if err := ReplaceIn(nsOf(t, pid), fresh, dir, false); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(inNS(pid, filepath.Join(dir, "new"))); err != nil || string(b) != "new" {
		t.Errorf("replacement in the namespace = %q, %v", b, err)
	}
}

func TestReplaceInRefusals(t *testing.T) {
	tmp := t.TempDir()
	fresh := filepath.Join(tmp, "fresh")
	dir := filepath.Join(tmp, "dir")
	hostTmpfs(t, fresh, "new")
	hostTmpfs(t, dir, "old")

	own := nsOf(t, os.Getpid())
	if err := ReplaceIn(own, fresh, dir, false); err == nil {
		t.Error("ReplaceIn into the namespace of the caller succeeded")
	}
	if _, err := os.Stat(filepath.Join(dir, "old")); err != nil {
		t.Errorf("the refused call changed the mount: %v", err)
	}
	// Not a mount namespace: setns refuses a namespace of another kind.
	if err := ReplaceIn(openNS(t, "/proc/self/ns/net"), fresh, dir, false); err == nil {
		t.Error("ReplaceIn into a network namespace succeeded")
	}
	if err := ReplaceIn(openNS(t, os.DevNull), fresh, dir, false); err == nil {
		t.Error("ReplaceIn into a file that is no namespace succeeded")
	}
	closed := openNS(t, "/proc/self/ns/mnt")
	closed.Close()
	if err := ReplaceIn(closed, fresh, dir, false); err == nil {
		t.Error("ReplaceIn into a closed file succeeded")
	}
	if err := ReplaceIn(own, filepath.Join(tmp, "missing"), dir, false); err == nil {
		t.Error("ReplaceIn from a missing source succeeded")
	}
}

// A target that is not in the namespace fails before the clone is attached, and
// leaves the mounts of the namespace as they were.
func TestReplaceInMissingTargetChangesNothing(t *testing.T) {
	tmp := t.TempDir()
	branch := filepath.Join(tmp, "branch")
	fresh := filepath.Join(tmp, "fresh")
	hostTmpfs(t, fresh, "new")
	hostTmpfs(t, branch, "old")
	pid := startNS(t, "plain", branch+"="+branch)
	missing := filepath.Join(tmp, "missing", "target")

	if err := ReplaceIn(nsOf(t, pid), fresh, missing, false); err == nil {
		t.Fatal("ReplaceIn of a target that does not exist succeeded")
	}
	if _, err := os.Stat(inNS(pid, missing)); !os.IsNotExist(err) {
		t.Errorf("the target exists in the namespace: %v", err)
	}
	if b, err := os.ReadFile(inNS(pid, filepath.Join(branch, "old"))); err != nil || string(b) != "old" {
		t.Errorf("the mount in the namespace = %q, %v; want it untouched", b, err)
	}
	if _, err := os.Stat(filepath.Join(fresh, "new")); err != nil {
		t.Errorf("the source on the host: %v", err)
	}
	// And the namespace still takes a replacement at a target that exists.
	if err := ReplaceIn(nsOf(t, pid), fresh, branch, false); err != nil {
		t.Errorf("ReplaceIn after the refused call: %v", err)
	}
}

// The namespace of the process as /proc shows it (that of its main thread) stays
// the one it was, however often a call moves a thread into another namespace.
func TestReplaceInLeavesTheProcessNamespace(t *testing.T) {
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "dir")
	fresh := filepath.Join(tmp, "fresh")
	hostTmpfs(t, fresh, "new")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	pid := startNS(t, "plain")
	before, err := os.Readlink("/proc/self/ns/mnt")
	if err != nil {
		t.Fatal(err)
	}
	for range 50 {
		if err := ReplaceIn(nsOf(t, pid), fresh, dir, false); err != nil {
			t.Fatal(err)
		}
	}
	if after, _ := os.Readlink("/proc/self/ns/mnt"); after != before {
		t.Errorf("namespace of the process = %s, was %s", after, before)
	}
}
