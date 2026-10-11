package daemon

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Ruakij/go-csi-fuse-supervisor/pkg/scope"
)

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// sh runs script; $1 is the mount point, which the script "mounts" by creating it.
func sh(script, mountPoint string) *exec.Cmd {
	return exec.Command("sh", "-c", script, "sh", mountPoint)
}

func waitClosed(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatal("channel was not closed")
	}
}

func TestStartReturnsOnceReady(t *testing.T) {
	mp := filepath.Join(t.TempDir(), "mp")
	exits := make(chan error, 1)
	d, err := Start(context.Background(), Config{
		Cmd:        sh(`sleep 0.3; touch "$1"; exec sleep 300`, mp),
		MountPoint: mp,
		Ready:      fileExists,
		Timeout:    10 * time.Second,
		OnExit:     func(err error) { exits <- err },
	})
	if err != nil {
		t.Fatal(err)
	}
	if !fileExists(mp) {
		t.Error("Start returned before the daemon was ready")
	}
	select {
	case <-d.Exited():
		t.Fatal("daemon has exited")
	default:
	}
	// Setsid: the daemon leads a session of its own.
	if sid, err := unix.Getsid(d.Pid()); err != nil || sid != d.Pid() {
		t.Errorf("session of the daemon = %d, %v; want its own pid %d", sid, err, d.Pid())
	}

	if err := syscall.Kill(d.Pid(), syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitClosed(t, d.Exited())
	select {
	case err := <-exits:
		if err == nil || !strings.Contains(err.Error(), "killed") {
			t.Errorf("OnExit err = %v, want the kill", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("OnExit was not called")
	}
}

func TestStartFailsWhenTheDaemonExitsFirst(t *testing.T) {
	var called atomic.Bool
	cmd := sh(`exit 3`, "unused")
	_, err := Start(context.Background(), Config{
		Cmd: cmd, MountPoint: filepath.Join(t.TempDir(), "mp"), Ready: fileExists,
		OnExit: func(error) { called.Store(true) },
	})
	if err == nil || !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("err = %v, want one with the exit status", err)
	}
	time.Sleep(100 * time.Millisecond)
	if called.Load() {
		t.Error("OnExit was called for a daemon Start gave up on")
	}
}

func TestStartKillsADaemonThatIsNotReadyInTime(t *testing.T) {
	cmd := sh(`exec sleep 300`, "unused")
	start := time.Now()
	_, err := Start(context.Background(), Config{
		Cmd: cmd, MountPoint: filepath.Join(t.TempDir(), "mp"), Ready: fileExists, Timeout: 300 * time.Millisecond,
	})
	if err == nil || !strings.Contains(err.Error(), "not ready within") {
		t.Fatalf("err = %v, want a timeout", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("Start took %s", time.Since(start))
	}
	waitProcessGone(t, cmd)
}

func TestStartKillsTheDaemonWhenTheContextIsDone(t *testing.T) {
	cmd := sh(`exec sleep 300`, "unused")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := Start(ctx, Config{Cmd: cmd, MountPoint: filepath.Join(t.TempDir(), "mp"), Ready: fileExists})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the context error", err)
	}
	waitProcessGone(t, cmd)
}

// The daemon leads a process group of its own, which is killed as a whole: what
// it started would otherwise outlive a daemon that Start gave up on.
func TestStartKillsTheProcessGroupOfADaemonItGivesUpOn(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	started := filepath.Join(t.TempDir(), "started")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Both the shell and its child inherit the pipe, and nothing else closes it.
	cmd := sh(`sleep 300 & touch "$1"; wait`, started)
	cmd.Stdout = pw
	_, err = Start(ctx, Config{Cmd: cmd, MountPoint: "/unused", Ready: func(string) bool {
		if fileExists(started) {
			cancel()
		}
		return false
	}})
	pw.Close()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the context error", err)
	}

	_ = pr.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := pr.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Errorf("read = %v, want EOF: a process of the daemon is still running", err)
	}
}

// A mount that is there before the daemon starts is not the daemon serving.
func TestDefaultReadyNeedsAMountOfAnotherDevice(t *testing.T) {
	dir := t.TempDir()
	live := func(string) bool { return true }
	ready := newMountReady(dir, live)
	if ready(dir) {
		t.Error("ready for the mount that was at the path before Start")
	}
	// /dev is a file system of its own, on Linux and on macOS.
	if !ready("/dev") {
		t.Error("not ready for a mount of another device")
	}
	if newMountReady(dir, func(string) bool { return false })("/dev") {
		t.Error("ready although the mount is not live")
	}
	// Nothing was at the path before: the first live mount counts.
	if !newMountReady(filepath.Join(dir, "missing"), live)(dir) {
		t.Error("not ready for a live mount at a path that did not exist")
	}
}

func TestStartFailsForACommandThatDoesNotStart(t *testing.T) {
	_, err := Start(context.Background(), Config{
		Cmd: exec.Command("/nonexistent/daemon"), MountPoint: "/x", Ready: func(string) bool { return true },
	})
	if err == nil {
		t.Fatal("Start of a missing binary succeeded")
	}
}

func TestStartRejectsBadConfigs(t *testing.T) {
	started := exec.Command("true")
	if err := started.Run(); err != nil {
		t.Fatal(err)
	}
	tests := map[string]Config{
		"no command":            {MountPoint: "/x"},
		"command started twice": {Cmd: started, MountPoint: "/x"},
		"no mount point":        {Cmd: exec.Command("true")},
	}
	for name, cfg := range tests {
		if _, err := Start(context.Background(), cfg); err == nil {
			t.Errorf("%s: Start succeeded", name)
		}
	}
}

// A manager that keeps daemons in the container does not get in the way.
func TestStartWithAnUnavailableScope(t *testing.T) {
	m, err := scope.New(context.Background(), scope.Config{Prefix: "test-", Lifetime: scope.InContainer})
	if err != nil {
		t.Fatal(err)
	}
	mp := filepath.Join(t.TempDir(), "mp")
	d, err := Start(context.Background(), Config{
		Cmd: sh(`touch "$1"; exec sleep 300`, mp), MountPoint: mp, Ready: fileExists, Scope: m, ID: "v",
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = syscall.Kill(d.Pid(), syscall.SIGKILL)
	waitClosed(t, d.Exited())

	if _, err := Start(context.Background(), Config{Cmd: exec.Command("true"), MountPoint: mp, Scope: m}); err == nil {
		t.Error("Start with a scope and no id succeeded")
	}
}

// waitProcessGone waits until the process of cmd is reaped, which the goroutine
// of Start does after the kill.
func waitProcessGone(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		// Signal 0 fails once the process is reaped; ESRCH, or the os level
		// "process already finished" for the handle.
		if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the process is still running")
}
