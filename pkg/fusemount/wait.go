package fusemount

import (
	"context"
	"fmt"
	"time"
)

// pollInterval is how often WaitMounted checks.
const pollInterval = 100 * time.Millisecond

// WaitMounted blocks until ready(path) reports true, and returns nil. It gives up
// with an error when exited is closed, which is how a daemon that died before it
// mounted is noticed, when timeout has passed (zero or less: never), or when ctx
// is done.
//
// A nil ready is [IsFUSE]; a daemon that mounts something else supplies its own
// check. ready is called about every 100 milliseconds, never twice at once. One
// that does not return, such as a statfs on a stuck FUSE mount, does not keep
// WaitMounted from returning at the timeout, but its goroutine lives on until
// the call returns. A nil exited never fires.
func WaitMounted(ctx context.Context, path string, exited <-chan struct{}, timeout time.Duration, ready func(string) bool) error {
	if ready == nil {
		ready = IsFUSE
	}
	var deadline <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		deadline = t.C
	}
	tick := time.NewTicker(pollInterval)
	defer tick.Stop()

	// result is nil while no check is running, so its case blocks.
	var result chan bool
	check := func() {
		r := make(chan bool, 1)
		result = r
		go func() { r <- ready(path) }()
	}
	check()
	for {
		select {
		case ok := <-result:
			result = nil
			if ok {
				return nil
			}
		case <-tick.C:
			if result == nil {
				check()
			}
		case <-exited:
			return fmt.Errorf("exited before %s was ready", path)
		case <-deadline:
			return fmt.Errorf("%s was not ready within %s", path, timeout)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
