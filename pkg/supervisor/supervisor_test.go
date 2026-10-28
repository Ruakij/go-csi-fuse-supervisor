package supervisor

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"
)

type recorder struct{ ch chan call }

type call struct {
	id    string
	first bool
}

func newRecorder() *recorder { return &recorder{ch: make(chan call, 100)} }

func (r *recorder) reconcile(_ context.Context, id string, first bool) {
	r.ch <- call{id, first}
}

func (r *recorder) next(t *testing.T) call {
	t.Helper()
	select {
	case c := <-r.ch:
		return c
	case <-time.After(10 * time.Second):
		t.Fatal("no reconcile call")
		return call{}
	}
}

// run starts the supervisor and returns a stop function that waits for Run.
func run(t *testing.T, s *Supervisor) (stop func()) {
	t.Helper()
	s.loop.MinGap = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Error("Run did not return")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

func TestPassReconcilesEveryIDInOrderAndMarksTheFirst(t *testing.T) {
	r := newRecorder()
	s, err := New(Config{
		IDs:       func() ([]string, error) { return []string{"b", "a"}, nil },
		Reconcile: r.reconcile,
	})
	if err != nil {
		t.Fatal(err)
	}
	run(t, s)

	for _, want := range []call{{"b", true}, {"a", true}} {
		if got := r.next(t); got != want {
			t.Errorf("call = %+v, want %+v", got, want)
		}
	}
	s.loop.Wake()
	for _, want := range []call{{"b", false}, {"a", false}} {
		if got := r.next(t); got != want {
			t.Errorf("call after a wake = %+v, want %+v", got, want)
		}
	}
}

// IDs may return what it could read together with the error about the rest: the
// error is reported, and what was read is reconciled all the same.
func TestIDsErrorGoesToOnErrorAndItsIDsAreReconciled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		boom := errors.New("boom")
		var errs []error
		r := newRecorder()
		s, _ := New(Config{
			IDs:       func() ([]string, error) { return []string{"a", "b"}, boom },
			Reconcile: r.reconcile,
			OnError:   func(err error) { errs = append(errs, err) },
		})
		stop := run(t, s)
		synctest.Wait()
		stop()

		if len(errs) != 1 || !errors.Is(errs[0], boom) {
			t.Errorf("errors = %v, want one that wraps %v", errs, boom)
		}
		for _, want := range []call{{"a", true}, {"b", true}} {
			if len(r.ch) == 0 {
				t.Fatalf("Reconcile(%q) was not called although IDs returned it", want.id)
			}
			if got := <-r.ch; got != want {
				t.Errorf("call = %+v, want %+v", got, want)
			}
		}
	})
}

func TestIDsErrorWithoutIDsReconcilesNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		boom := errors.New("boom")
		var errs []error
		r := newRecorder()
		s, _ := New(Config{
			IDs:       func() ([]string, error) { return nil, boom },
			Reconcile: r.reconcile,
			OnError:   func(err error) { errs = append(errs, err) },
		})
		stop := run(t, s)
		synctest.Wait()
		stop()

		if len(errs) != 1 || !errors.Is(errs[0], boom) {
			t.Errorf("errors = %v, want one that wraps %v", errs, boom)
		}
		if len(r.ch) != 0 {
			t.Error("Reconcile was called although IDs returned no id")
		}
	})
}

func TestNewRequiresIDsAndReconcile(t *testing.T) {
	if _, err := New(Config{Reconcile: newRecorder().reconcile}); err == nil {
		t.Error("New without IDs succeeded")
	}
	if _, err := New(Config{IDs: func() ([]string, error) { return nil, nil }}); err == nil {
		t.Error("New without Reconcile succeeded")
	}
}

// A repair pass and a CSI call that hold the lock of the same volume exclude
// each other, other volumes are not held up.
func TestReconcileHoldsTheLockOfTheID(t *testing.T) {
	inside := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	s, _ := New(Config{
		IDs: func() ([]string, error) { return []string{"a"}, nil },
		Reconcile: func(context.Context, string, bool) {
			once.Do(func() {
				close(inside)
				<-release
			})
		},
	})
	run(t, s)
	<-inside

	locked := make(chan struct{})
	go func() { s.Lock("a"); close(locked); s.Unlock("a") }()
	select {
	case <-locked:
		t.Fatal("Lock(a) succeeded while Reconcile(a) runs")
	case <-time.After(100 * time.Millisecond):
	}
	s.Lock("b")
	s.Unlock("b")

	close(release)
	select {
	case <-locked:
	case <-time.After(10 * time.Second):
		t.Fatal("Lock(a) did not get the lock after Reconcile")
	}
}

func TestStartedDaemonWakesTheLoopWhenItExits(t *testing.T) {
	r := newRecorder()
	s, _ := New(Config{
		IDs:       func() ([]string, error) { return []string{"v"}, nil },
		Reconcile: r.reconcile,
	})
	run(t, s)
	r.next(t) // the first pass

	mp := filepath.Join(t.TempDir(), "mp")
	exited := make(chan error, 1)
	cmd := exec.Command("sh", "-c", `touch "$1"; exec sleep 300`, "sh", mp)
	d, err := s.Start(context.Background(), "v", cmd, mp, StartOptions{
		Ready:   func(p string) bool { _, err := os.Stat(p); return err == nil },
		Timeout: 10 * time.Second,
		OnExit:  func(err error) { exited <- err },
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.PID("v"); got != d.Pid() {
		t.Errorf("PID = %d, want %d", got, d.Pid())
	}
	if got := s.PID("other"); got != 0 {
		t.Errorf("PID of an unknown volume = %d, want 0", got)
	}
	if len(r.ch) != 0 {
		t.Fatal("a pass ran before the daemon exited")
	}

	if err := syscall.Kill(d.Pid(), syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-exited:
		if err == nil {
			t.Error("OnExit error = nil, want the kill")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("OnExit not called")
	}
	if got := r.next(t); got != (call{"v", false}) {
		t.Errorf("call = %+v, want a pass for v", got)
	}
	if got := s.PID("v"); got != 0 {
		t.Errorf("PID after the exit = %d, want 0", got)
	}
}

func TestFailedStartIsNotRemembered(t *testing.T) {
	r := newRecorder()
	s, _ := New(Config{
		IDs:       func() ([]string, error) { return nil, nil },
		Reconcile: r.reconcile,
	})
	cmd := exec.Command("sh", "-c", "exit 1")
	if _, err := s.Start(context.Background(), "v", cmd, "/nonexistent", StartOptions{Timeout: 5 * time.Second}); err == nil {
		t.Fatal("Start succeeded")
	}
	if got := s.PID("v"); got != 0 {
		t.Errorf("PID = %d, want 0", got)
	}
}

func TestReplaceInNeedsADaemon(t *testing.T) {
	s, _ := New(Config{
		IDs:       func() ([]string, error) { return nil, nil },
		Reconcile: newRecorder().reconcile,
	})
	if err := s.ReplaceIn("none", "/a", "/b", false); err == nil {
		t.Error("ReplaceIn of a volume without a daemon succeeded")
	}
}
