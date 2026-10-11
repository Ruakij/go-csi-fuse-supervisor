//go:build linux && mounttest

package supervisor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// The daemon mounts in a mount namespace of its own, as a sandboxed union daemon
// does, and ReplaceIn works in that namespace through the pid of the volume.
func TestReplaceInTheNamespaceOfTheDaemon(t *testing.T) {
	tmp := t.TempDir()
	mp := filepath.Join(tmp, "branch")
	fresh := filepath.Join(tmp, "fresh")
	for _, dir := range []string{mp, fresh} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(fresh, "new"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := New(Config{
		IDs:       func() ([]string, error) { return nil, nil },
		Reconcile: newRecorder().reconcile,
	})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("unshare", "-m", "sh", "-c", `mount -t tmpfs tmpfs "$1" && touch "$1/old" && exec sleep 300`, "sh", mp)
	inDaemon := func(path string) string { return fmt.Sprintf("/proc/%d/root%s", cmd.Process.Pid, path) }
	d, err := s.Start(context.Background(), "v", cmd, mp, StartOptions{
		Ready:   func(string) bool { _, err := os.Stat(inDaemon(filepath.Join(mp, "old"))); return err == nil },
		Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(d.Pid(), syscall.SIGKILL) })

	if err := s.ReplaceIn("v", fresh, mp, false); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(inDaemon(filepath.Join(mp, "new"))); err != nil || string(b) != "new" {
		t.Errorf("replacement in the daemon = %q, %v", b, err)
	}
	if _, err := os.Stat(inDaemon(filepath.Join(mp, "old"))); !os.IsNotExist(err) {
		t.Errorf("the old mount shows through: %v", err)
	}
	if _, err := os.Stat(filepath.Join(mp, "new")); !os.IsNotExist(err) {
		t.Errorf("the replacement reached the namespace of the caller: %v", err)
	}

	// Once the daemon is gone its pid may belong to anyone: nothing is done.
	if err := syscall.Kill(d.Pid(), syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	select {
	case <-d.Exited():
	case <-time.After(10 * time.Second):
		t.Fatal("the daemon did not exit")
	}
	if err := s.ReplaceIn("v", fresh, mp, false); err == nil {
		t.Error("ReplaceIn after the daemon exited succeeded")
	}
}
