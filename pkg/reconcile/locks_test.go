package reconcile

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLocksExcludeTheSameID(t *testing.T) {
	var l Locks
	var inside, maxInside atomic.Int32
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			l.Lock("vol")
			n := inside.Add(1)
			if n > maxInside.Load() {
				maxInside.Store(n)
			}
			time.Sleep(time.Millisecond)
			inside.Add(-1)
			l.Unlock("vol")
		})
	}
	wg.Wait()
	if got := maxInside.Load(); got != 1 {
		t.Errorf("%d holders at once, want 1", got)
	}
}

// Ids that a hashed lock set could put in one bucket must not wait for each other.
func TestLocksDoNotContendAcrossIDs(t *testing.T) {
	var l Locks
	l.Lock("a")
	defer l.Unlock("a")

	got := make(chan struct{})
	go func() {
		l.Lock("b")
		l.Unlock("b")
		close(got)
	}()
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("locking b waited for a")
	}
}

func TestLockWaitsUntilUnlock(t *testing.T) {
	var l Locks
	l.Lock("a")
	acquired := make(chan struct{})
	go func() {
		l.Lock("a")
		close(acquired)
		l.Unlock("a")
	}()
	select {
	case <-acquired:
		t.Fatal("second Lock returned while the first was held")
	case <-time.After(50 * time.Millisecond):
	}
	l.Unlock("a")
	select {
	case <-acquired:
	case <-time.After(5 * time.Second):
		t.Fatal("second Lock did not return after Unlock")
	}
}

func TestLocksDropIdleEntries(t *testing.T) {
	var l Locks
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Go(func() {
			id := string(rune('a' + i%5))
			l.Lock(id)
			l.Unlock(id)
		})
	}
	wg.Wait()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.m) != 0 {
		t.Errorf("%d entries left after every Unlock, want 0", len(l.m))
	}
}

func TestUnlockOfUnlockedIDPanics(t *testing.T) {
	var l Locks
	defer func() {
		if recover() == nil {
			t.Error("Unlock of an id that was never locked did not panic")
		}
	}()
	l.Unlock("a")
}
