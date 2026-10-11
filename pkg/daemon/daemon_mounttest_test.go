//go:build linux && mounttest

package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Ruakij/go-csi-fuse-supervisor/pkg/scope"
)

const tmpfsMagic = 0x01021994

func isTmpfs(path string) bool {
	var st unix.Statfs_t
	return unix.Statfs(path, &st) == nil && uint64(st.Type) == tmpfsMagic
}

// A daemon that mounts something other than FUSE is ready by a check of its own.
// The mount belongs to the process that made it, here a shell in its own mount
// namespace, so nothing is left behind on the host.
func mounter(mp string) *exec.Cmd {
	return exec.Command("unshare", "-m", "sh", "-c", `mount -t tmpfs tmpfs "$1" && exec sleep 300`, "sh", mp)
}

func TestStartWithAMountingDaemon(t *testing.T) {
	// The daemon mounts in a namespace of its own, so the host does not see it and
	// the check runs through the proc root of the daemon.
	mp := filepath.Join(t.TempDir(), "mp")
	if err := os.Mkdir(mp, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := mounter(mp)
	var pid int
	d, err := Start(context.Background(), Config{
		Cmd:        cmd,
		MountPoint: mp,
		Timeout:    10 * time.Second,
		Ready: func(string) bool {
			pid = cmd.Process.Pid
			return isTmpfs("/proc/" + strconv.Itoa(pid) + "/root" + mp)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if d.Pid() != pid {
		t.Errorf("Pid = %d, want %d", d.Pid(), pid)
	}
	_ = syscall.Kill(d.Pid(), syscall.SIGKILL)
	select {
	case <-d.Exited():
	case <-time.After(10 * time.Second):
		t.Fatal("daemon did not exit")
	}
}

// The daemon is in the scope of its volume when Start returns, and the scope
// outlives the context of the call. Needs the systemd of the host, see
// make test-mount-systemd.
func TestStartIntoASystemdScope(t *testing.T) {
	if os.Getenv("MOUNTTEST_SYSTEMD") == "" {
		t.Skip("MOUNTTEST_SYSTEMD is not set, see make test-mount-systemd")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	m, err := scope.New(ctx, scope.Config{Prefix: "go-csi-fuse-supervisor-test-", Lifetime: scope.Systemd})
	if err != nil {
		t.Fatal(err)
	}
	id := "daemon-test-" + time.Now().Format("150405.000")
	mp := filepath.Join(t.TempDir(), "mp")
	d, err := Start(ctx, Config{
		Cmd:        exec.Command("sh", "-c", `touch "$1"; exec sleep 300`, "sh", mp),
		MountPoint: mp,
		Ready:      func(p string) bool { _, err := os.Stat(p); return err == nil },
		Scope:      m,
		ID:         id,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Stop(context.Background(), id); _ = syscall.Kill(d.Pid(), syscall.SIGKILL) })
	cancel()

	if got := m.PID(id); got != d.Pid() {
		t.Errorf("PID of the scope = %d, want %d", got, d.Pid())
	}
	select {
	case <-d.Exited():
		t.Fatal("the daemon exited with the context")
	case <-time.After(300 * time.Millisecond):
	}
}
