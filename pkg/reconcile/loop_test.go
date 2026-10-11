package reconcile

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// The tests run in a synctest bubble: time is fake and moves only while every
// goroutine waits, so the intervals and gaps are exact and nothing sleeps for real.

// recorder collects the start time and first flag of every pass.
type recorder struct {
	mu     sync.Mutex
	starts []time.Time
	firsts []bool
	// block, when set, holds every pass until it is closed.
	block chan struct{}
}

func (r *recorder) pass(_ context.Context, first bool) {
	r.mu.Lock()
	r.starts = append(r.starts, time.Now())
	r.firsts = append(r.firsts, first)
	block := r.block
	r.mu.Unlock()
	if block != nil {
		<-block
	}
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.starts)
}

// since returns the start of every pass relative to begin.
func (r *recorder) since(begin time.Time) []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	d := make([]time.Duration, len(r.starts))
	for i, s := range r.starts {
		d[i] = s.Sub(begin)
	}
	return d
}

// start runs the loop and returns a stop function that waits for Run to return.
func start(l *Loop) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		l.Run(ctx)
		close(done)
	}()
	return func() {
		cancel()
		<-done
	}
}

func TestFirstPassIsImmediateAndMarkedFirst(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := &recorder{}
		l := &Loop{Interval: time.Hour, MinGap: time.Hour, Pass: r.pass}
		begin := time.Now()
		stop := start(l)
		defer stop()

		synctest.Wait()
		got := r.since(begin)
		if len(got) != 1 || got[0] != 0 {
			t.Fatalf("passes at %v, want one at once despite MinGap", got)
		}
		if !r.firsts[0] {
			t.Error("first pass got first=false")
		}
	})
}

func TestWakeRunsAnotherPassWithFirstFalse(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := &recorder{}
		l := &Loop{Interval: time.Hour, MinGap: time.Millisecond, Pass: r.pass}
		stop := start(l)
		defer stop()
		synctest.Wait()

		l.Wake()
		time.Sleep(time.Millisecond)
		synctest.Wait()
		r.mu.Lock()
		defer r.mu.Unlock()
		if len(r.firsts) != 2 || !r.firsts[0] || r.firsts[1] {
			t.Errorf("firsts = %v, want [true false]", r.firsts)
		}
	})
}

func TestIntervalRunsPassesWithoutWakes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := &recorder{}
		l := &Loop{Interval: 20 * time.Millisecond, MinGap: time.Millisecond, Pass: r.pass}
		begin := time.Now()
		stop := start(l)
		defer stop()

		time.Sleep(70 * time.Millisecond)
		synctest.Wait()
		got := r.since(begin)
		want := []time.Duration{0, 20 * time.Millisecond, 40 * time.Millisecond, 60 * time.Millisecond}
		if len(got) != len(want) {
			t.Fatalf("passes at %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("passes at %v, want %v", got, want)
				break
			}
		}
	})
}

// A burst of wakes during a pass is one pending wake, so one more pass runs.
func TestWakeBurstCollapsesIntoOnePass(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := &recorder{block: make(chan struct{})}
		l := &Loop{Interval: time.Hour, MinGap: time.Millisecond, Pass: r.pass}
		stop := start(l)
		defer stop()

		synctest.Wait()
		if got := r.count(); got != 1 {
			t.Fatalf("%d passes before the burst, want the first one, held in block", got)
		}
		for range 100 {
			l.Wake()
		}
		close(r.block)

		time.Sleep(time.Second)
		synctest.Wait()
		if got := r.count(); got != 2 {
			t.Errorf("%d passes, want 2: the first and the one the burst asked for", got)
		}
	})
}

// Wake before Run is kept, and also must not block.
func TestWakeBeforeRunIsKept(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := &recorder{}
		l := &Loop{Interval: time.Hour, MinGap: time.Millisecond, Pass: r.pass}
		for range 10 {
			l.Wake()
		}
		stop := start(l)
		defer stop()

		time.Sleep(10 * time.Millisecond)
		synctest.Wait()
		if got := r.count(); got != 2 {
			t.Errorf("%d passes, want 2: the first and the one the wake asked for", got)
		}
	})
}

func TestMinGapSpacesPassesAfterWakes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const gap = 150 * time.Millisecond
		r := &recorder{}
		l := &Loop{Interval: time.Hour, MinGap: gap, Pass: r.pass}
		begin := time.Now()
		stop := start(l)
		defer stop()
		synctest.Wait()

		// A daemon dying right after each remount wakes the loop again at once.
		for range 3 {
			l.Wake()
			time.Sleep(gap)
			synctest.Wait()
		}
		got := r.since(begin)
		want := []time.Duration{0, gap, 2 * gap, 3 * gap}
		if len(got) != len(want) {
			t.Fatalf("passes at %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("passes at %v, want %v", got, want)
				break
			}
		}
	})
}

func TestRunReturnsWhenContextIsDoneWhileIdle(t *testing.T) {
	synctest.Test(t, func(_ *testing.T) {
		r := &recorder{}
		l := &Loop{Interval: time.Hour, MinGap: time.Millisecond, Pass: r.pass}
		stop := start(l)
		synctest.Wait()
		stop()
	})
}

func TestRunReturnsWhenContextIsDoneWhileWaitingForTheGap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := &recorder{}
		l := &Loop{Interval: time.Hour, MinGap: time.Hour, Pass: r.pass}
		stop := start(l)
		synctest.Wait()
		l.Wake() // the loop waits out the gap
		time.Sleep(20 * time.Millisecond)
		synctest.Wait()
		stop()
		if got := r.count(); got != 1 {
			t.Errorf("%d passes, want 1: a pass ran after the context was done", got)
		}
	})
}

func TestRunWithCancelledContextRunsNoPass(t *testing.T) {
	r := &recorder{}
	l := &Loop{Pass: r.pass}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	l.Run(ctx)
	if got := r.count(); got != 0 {
		t.Errorf("%d passes, want 0", got)
	}
}
