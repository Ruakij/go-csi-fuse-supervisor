package fusemount

import (
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// The tests that wait for the probe timeout run in a synctest bubble, where time
// is fake and moves only while every goroutine waits.
func TestProberReportsAHungPathWithoutWaitingAgain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		block := make(chan struct{})
		var calls atomic.Int32
		p := NewProber(10 * time.Millisecond)
		p.call = func(string) (State, error) {
			calls.Add(1)
			<-block
			return Live, nil
		}

		start := time.Now()
		if _, err := p.Probe("/branch"); !errors.Is(err, ErrHung) {
			t.Fatalf("Probe() = %v, want %v", err, ErrHung)
		}
		// The second probe must not wait for the timeout again, nor start a second call.
		if state, err := p.Probe("/branch"); !errors.Is(err, ErrHung) || state != Other {
			t.Fatalf("second Probe() = %v, %v; want other, %v", state, err, ErrHung)
		}
		if took := time.Since(start); took != p.timeout {
			t.Errorf("two probes took %v, want one timeout, %v", took, p.timeout)
		}
		if got := calls.Load(); got != 1 {
			t.Errorf("%d calls, want 1: the hung path was probed again", got)
		}

		// Another path is not affected.
		other := NewProber(10 * time.Millisecond)
		other.call = func(string) (State, error) { return Dead, errors.New("ENOTCONN") }
		if state, err := other.Probe("/else"); state != Dead || err == nil {
			t.Errorf("Probe of another path = %v, %v; want dead and its error", state, err)
		}

		// Once the stuck call returns, the path is probed again.
		close(block)
		synctest.Wait()
		if state, err := p.Probe("/branch"); err != nil || state != Live {
			t.Errorf("Probe after the stuck call returned = %v, %v; want live", state, err)
		}
	})
}

func TestProberPassesAnswersThrough(t *testing.T) {
	want := errors.New("boom")
	p := NewProber(time.Second)
	p.call = func(string) (State, error) { return Dead, want }
	if state, err := p.Probe("/x"); state != Dead || !errors.Is(err, want) {
		t.Errorf("Probe = %v, %v; want dead, %v", state, err, want)
	}
}

func TestProberForgetProbesAgainAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		block := make(chan struct{})
		defer close(block)
		p := NewProber(10 * time.Millisecond)
		p.call = func(string) (State, error) {
			if calls.Add(1) == 1 {
				<-block
			}
			return Live, nil
		}

		if _, err := p.Probe("/m"); !errors.Is(err, ErrHung) {
			t.Fatalf("Probe() = %v, want %v", err, ErrHung)
		}
		p.Forget("/m")
		// The first call is still stuck, on the old mount; the new one is probed.
		if state, err := p.Probe("/m"); state != Live || err != nil {
			t.Errorf("Probe after Forget = %v, %v; want live", state, err)
		}
	})
}

func TestNewProberDefaultsTheTimeout(t *testing.T) {
	if got := NewProber(0).timeout; got != defaultProbeTimeout {
		t.Errorf("timeout = %v, want %v", got, defaultProbeTimeout)
	}
}
