//go:build linux && mounttest

package scope

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

// Runs against the systemd of the host: make test-mount-systemd hands in its
// socket and the pids of the host.
func TestSystemdScope(t *testing.T) {
	if os.Getenv("MOUNTTEST_SYSTEMD") == "" {
		t.Skip("MOUNTTEST_SYSTEMD is not set, see make test-mount-systemd")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	m, err := New(ctx, Config{Prefix: "go-csi-fuse-supervisor-test-", Lifetime: Systemd, TasksMax: 100})
	if err != nil {
		t.Fatal(err)
	}
	id := "scope-test/" + time.Now().Format("150405.000")

	unit, err := m.Unit(id)
	if err != nil {
		t.Fatal(err)
	}
	stopped := make(chan string, 8)
	if err := m.Watch(ctx, func(unit string) { stopped <- unit }); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("sleep", "300")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Stop(context.Background(), id); _ = cmd.Process.Kill(); _ = cmd.Wait() })

	if err := m.Adopt(ctx, id, cmd.Process.Pid); err != nil {
		t.Fatal(err)
	}
	if got := m.PID(id); got != cmd.Process.Pid {
		t.Errorf("PID = %d, want %d", got, cmd.Process.Pid)
	}

	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case u := <-stopped:
			if u == unit {
				if got := m.PID(id); got != 0 {
					t.Errorf("PID after the exit = %d, want 0", got)
				}
				// Gone already, which is not an error.
				if err := m.Stop(ctx, id); err != nil {
					t.Errorf("Stop of a scope that is gone: %v", err)
				}
				return
			}
		case <-deadline:
			t.Fatal("Watch did not report the scope stopping")
		}
	}
}

// Stop returns when the scope is stopped, so the id can be adopted again at once
// without systemd refusing the name of a scope that is still there.
func TestSystemdStopThenAdopt(t *testing.T) {
	if os.Getenv("MOUNTTEST_SYSTEMD") == "" {
		t.Skip("MOUNTTEST_SYSTEMD is not set, see make test-mount-systemd")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	m, err := New(ctx, Config{Prefix: "go-csi-fuse-supervisor-test-", Lifetime: Systemd})
	if err != nil {
		t.Fatal(err)
	}
	id := "stop-test-" + time.Now().Format("150405.000")
	t.Cleanup(func() { _ = m.Stop(context.Background(), id) })

	for i := range 5 {
		cmd := exec.Command("sleep", "300")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		if err := m.Adopt(ctx, id, cmd.Process.Pid); err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatalf("Adopt #%d: %v", i, err)
		}
		if err := m.Stop(ctx, id); err != nil {
			t.Fatalf("Stop #%d: %v", i, err)
		}
		// Stopped, so the process is gone.
		if err := cmd.Wait(); err == nil {
			t.Errorf("process #%d exited cleanly, want it killed by the stop", i)
		}
		if got := m.PID(id); got != 0 {
			t.Errorf("PID after Stop #%d = %d, want 0", i, got)
		}
	}
}

func TestSystemdUnreachable(t *testing.T) {
	if os.Getenv("MOUNTTEST_SYSTEMD") != "" {
		t.Skip("systemd is reachable")
	}
	// The mount test container has no systemd socket; Auto then falls back.
	m, err := New(context.Background(), Config{Prefix: "go-csi-fuse-supervisor-test-"})
	if err != nil {
		t.Fatal(err)
	}
	if m.Available() || m.FellBack() == nil {
		t.Errorf("Available = %v, FellBack = %v; want a fallback without systemd", m.Available(), m.FellBack())
	}
	if _, err := New(context.Background(), Config{Prefix: "go-csi-fuse-supervisor-test-", Lifetime: Systemd}); err == nil {
		t.Error("Systemd without systemd succeeded")
	}
}
