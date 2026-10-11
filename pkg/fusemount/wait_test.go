package fusemount

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// The tests that wait run in a synctest bubble, where time is fake and moves only
// while every goroutine waits, so the poll interval and timeouts are exact.

func TestWaitMountedReturnsOnceReady(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		ready := func(string) bool { return calls.Add(1) >= 3 }
		start := time.Now()
		if err := WaitMounted(context.Background(), "/m", nil, 10*time.Second, ready); err != nil {
			t.Fatal(err)
		}
		if got := calls.Load(); got != 3 {
			t.Errorf("ready called %d times, want 3", got)
		}
		if took := time.Since(start); took != 2*pollInterval {
			t.Errorf("took %v, want two poll intervals", took)
		}
	})
}

func TestWaitMountedPassesThePathToReady(t *testing.T) {
	var got string
	ready := func(p string) bool { got = p; return true }
	if err := WaitMounted(context.Background(), "/mnt/x", nil, time.Second, ready); err != nil {
		t.Fatal(err)
	}
	if got != "/mnt/x" {
		t.Errorf("ready got %q, want /mnt/x", got)
	}
}

func TestWaitMountedFailsWhenTheDaemonExits(t *testing.T) {
	exited := make(chan struct{})
	close(exited)
	err := WaitMounted(context.Background(), "/m", exited, 10*time.Second, func(string) bool { return false })
	if err == nil || !strings.Contains(err.Error(), "exited") {
		t.Errorf("err = %v, want an exit error", err)
	}
}

func TestWaitMountedTimesOut(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		err := WaitMounted(context.Background(), "/m", nil, 50*time.Millisecond, func(string) bool { return false })
		if err == nil || !strings.Contains(err.Error(), "not ready within") {
			t.Errorf("err = %v, want a timeout", err)
		}
		if took := time.Since(start); took != 50*time.Millisecond {
			t.Errorf("took %v, want the timeout", took)
		}
	})
}

func TestWaitMountedStopsWithTheContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(30 * time.Millisecond)
			cancel()
		}()
		err := WaitMounted(ctx, "/m", nil, 0, func(string) bool { return false })
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	})
}

// A statfs on a stuck FUSE mount never returns, and must not keep the wait from
// giving up.
func TestWaitMountedGivesUpWhileReadyIsStuck(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		block := make(chan struct{})
		defer close(block)
		start := time.Now()
		err := WaitMounted(context.Background(), "/m", nil, 100*time.Millisecond, func(string) bool {
			<-block
			return true
		})
		if err == nil {
			t.Fatal("err = nil, want a timeout")
		}
		if took := time.Since(start); took != 100*time.Millisecond {
			t.Errorf("took %v, want the timeout", took)
		}
	})
}

func TestWaitMountedChecksOneAtATime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var running, maxRunning atomic.Int32
		var calls atomic.Int32
		ready := func(string) bool {
			n := running.Add(1)
			if n > maxRunning.Load() {
				maxRunning.Store(n)
			}
			time.Sleep(250 * time.Millisecond) // longer than two poll intervals
			running.Add(-1)
			return calls.Add(1) >= 2
		}
		if err := WaitMounted(context.Background(), "/m", nil, 10*time.Second, ready); err != nil {
			t.Fatal(err)
		}
		if got := maxRunning.Load(); got != 1 {
			t.Errorf("%d checks at once, want 1", got)
		}
	})
}
