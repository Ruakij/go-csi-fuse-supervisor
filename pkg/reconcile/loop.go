// Package reconcile runs repair passes on a timer and on demand, and serializes
// them with the calls that change the same volumes.
package reconcile

import (
	"context"
	"sync"
	"time"
)

const (
	defaultInterval = 30 * time.Second
	defaultMinGap   = time.Second
)

// Loop calls Pass repeatedly: once at the start, then every Interval and
// whenever Wake asks for one. The zero value, with Pass set, is ready to use.
type Loop struct {
	// Interval is the time between passes when nothing wakes the loop. It is the
	// backstop for events that were missed. Zero means 30 seconds.
	Interval time.Duration
	// MinGap is the least time between the starts of two passes. A daemon that
	// dies right after it was mounted wakes the loop again at once, and without
	// a gap that would be a tight remount loop. Zero means one second.
	MinGap time.Duration
	// Pass repairs whatever it finds broken. first is true in the first pass only,
	// which is the one that finds what an earlier process left running.
	Pass func(ctx context.Context, first bool)

	once sync.Once
	wake chan struct{}
}

func (l *Loop) init() {
	l.once.Do(func() { l.wake = make(chan struct{}, 1) })
}

// Wake cuts the wait for the next pass short. At most one wake is pending, so a
// burst of them, such as several daemons exiting together, collapses into one
// pass. A wake during a pass is kept for the pass after it. Wake never blocks.
func (l *Loop) Wake() {
	l.init()
	select {
	case l.wake <- struct{}{}:
	default:
	}
}

// Run calls Pass until ctx is done, and then returns. The first pass starts
// immediately.
func (l *Loop) Run(ctx context.Context) {
	l.init()
	interval, minGap := l.Interval, l.MinGap
	if interval <= 0 {
		interval = defaultInterval
	}
	if minGap <= 0 {
		minGap = defaultMinGap
	}

	tick := time.NewTicker(interval)
	defer tick.Stop()
	var last time.Time
	for first := true; ; first = false {
		if wait := minGap - time.Since(last); wait > 0 {
			t := time.NewTimer(wait)
			select {
			case <-t.C:
			case <-ctx.Done():
				t.Stop()
				return
			}
		}
		if ctx.Err() != nil {
			return
		}
		last = time.Now()
		l.Pass(ctx, first)
		select {
		case <-tick.C:
		case <-l.wake:
		case <-ctx.Done():
			return
		}
	}
}
