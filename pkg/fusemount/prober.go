package fusemount

import (
	"errors"
	"sync"
	"time"
)

// ErrHung is returned by [Prober.Probe] for a path that does not answer.
var ErrHung = errors.New("statfs did not return in time")

const defaultProbeTimeout = 5 * time.Second

// Prober classifies paths like [Status] but gives up on one that does not answer.
// A FUSE mount whose daemon is gone fails at once with ENOTCONN, but one whose
// daemon is alive and stuck never answers, and a statfs stuck in the kernel
// cannot be cancelled. The path then counts as hung, and is not probed again,
// until the stuck call returns, so repeated passes do not pile up threads.
//
// A Prober is safe for concurrent use.
type Prober struct {
	timeout time.Duration
	call    func(path string) (State, error)

	mu   sync.Mutex
	hung map[string]uint64
	gen  uint64
}

// NewProber returns a Prober that waits for timeout (zero or less: 5 seconds).
func NewProber(timeout time.Duration) *Prober {
	if timeout <= 0 {
		timeout = defaultProbeTimeout
	}
	return &Prober{timeout: timeout, call: statfsState, hung: map[string]uint64{}}
}

// Probe returns what path is and the error statfs failed with, if it did. A
// path that does not answer within the timeout, or whose earlier call has not
// returned yet, is Other with [ErrHung].
func (p *Prober) Probe(path string) (State, error) {
	p.mu.Lock()
	_, hung := p.hung[path]
	p.mu.Unlock()
	if hung {
		return Other, ErrHung
	}

	type result struct {
		state State
		err   error
	}
	done := make(chan result, 1)
	go func() {
		s, err := p.call(path)
		done <- result{s, err}
	}()
	t := time.NewTimer(p.timeout)
	defer t.Stop()
	select {
	case r := <-done:
		return r.state, r.err
	case <-t.C:
	}

	p.mu.Lock()
	p.gen++
	gen := p.gen
	p.hung[path] = gen
	p.mu.Unlock()
	go func() {
		<-done
		p.mu.Lock()
		// Forget may have dropped the mark, and a later probe set a new one.
		if p.hung[path] == gen {
			delete(p.hung, path)
		}
		p.mu.Unlock()
	}()
	return Other, ErrHung
}

// Forget drops the hung mark of path, for when what is mounted there has
// changed and the stuck call belongs to the old mount. The call itself lives on
// until it returns.
func (p *Prober) Forget(path string) {
	p.mu.Lock()
	delete(p.hung, path)
	p.mu.Unlock()
}
