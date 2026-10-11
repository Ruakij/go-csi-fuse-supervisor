// Package supervisor ties the packages of this module together for a CSI driver
// whose volumes are served by FUSE daemons: it starts the daemons, and keeps
// calling the driver to repair the volumes whose daemon died or went stale.
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/Ruakij/go-csi-fuse-supervisor/pkg/daemon"
	"github.com/Ruakij/go-csi-fuse-supervisor/pkg/fusemount"
	"github.com/Ruakij/go-csi-fuse-supervisor/pkg/reconcile"
	"github.com/Ruakij/go-csi-fuse-supervisor/pkg/scope"
)

// Config configures a [Supervisor].
type Config struct {
	// Scope, if set, holds the daemons in systemd scopes, and wakes the
	// supervisor when one stops.
	Scope *scope.Manager
	// IDs returns the ids of all volumes that have a daemon to look after, in the
	// order to reconcile them. It is called at the start of each pass. It may
	// return ids together with an error, such as the volumes that could be read
	// next to those that could not: the error goes to OnError and the ids are
	// reconciled all the same.
	IDs func() ([]string, error)
	// Reconcile checks one volume and repairs it. It runs with the lock of the id
	// held, so it does not overlap with a CSI call that holds [Supervisor.Lock]
	// for the same volume, and should load what it needs again under the lock:
	// the volume may have been unpublished since IDs ran. first is true on the
	// first pass after the driver started, when the volumes are the ones an earlier
	// driver left behind.
	Reconcile func(ctx context.Context, id string, first bool)
	// OnError receives the errors that have no caller to return them to: the
	// errors of IDs and of the watch on the scopes. Default: ignored.
	OnError func(error)
}

// Supervisor runs the repair passes. Create it with [New].
type Supervisor struct {
	cfg   Config
	loop  reconcile.Loop
	locks reconcile.Locks

	mu      sync.Mutex
	started map[string]*daemon.Daemon
}

// New returns a Supervisor. It does nothing until [Supervisor.Run].
func New(cfg Config) (*Supervisor, error) {
	if cfg.IDs == nil || cfg.Reconcile == nil {
		return nil, errors.New("IDs and Reconcile are required")
	}
	s := &Supervisor{cfg: cfg, started: map[string]*daemon.Daemon{}}
	s.loop.Pass = s.pass
	return s, nil
}

// Run reconciles every 30 seconds, and as soon as a daemon exits, until ctx is
// done. It starts with a pass, and passes are at least a second apart, so a
// daemon that dies right after it started cannot cause a tight loop of remounts.
//
// Daemons started by this process report their exit themselves. Those started by
// an earlier driver are seen to exit only by watching host systemd; when that
// fails, the next pass finds them.
func (s *Supervisor) Run(ctx context.Context) {
	if s.cfg.Scope != nil {
		err := s.cfg.Scope.Watch(ctx, func(string) { s.loop.Wake() })
		if err != nil {
			s.onError(fmt.Errorf("cannot watch host systemd for daemon exits, repairs wait for the next pass: %w", err))
		}
	}
	s.loop.Run(ctx)
}

func (s *Supervisor) pass(ctx context.Context, first bool) {
	ids, err := s.cfg.IDs()
	if err != nil {
		s.onError(fmt.Errorf("list volumes: %w", err))
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		s.reconcile(ctx, id, first)
	}
}

func (s *Supervisor) reconcile(ctx context.Context, id string, first bool) {
	s.locks.Lock(id)
	defer s.locks.Unlock(id)
	s.cfg.Reconcile(ctx, id, first)
}

func (s *Supervisor) onError(err error) {
	if s.cfg.OnError != nil {
		s.cfg.OnError(err)
	}
}

// Lock takes the lock of volume id, the one a repair pass holds while it
// reconciles that volume. CSI calls that change a volume hold it, so that a repair
// does not remount a volume in the middle of its unpublish. Locks of different ids
// are independent. Lock is not reentrant, and Reconcile runs with it held.
func (s *Supervisor) Lock(id string) { s.locks.Lock(id) }

// Unlock releases the lock taken by [Supervisor.Lock].
func (s *Supervisor) Unlock(id string) { s.locks.Unlock(id) }

// StartOptions are the optional parts of [Supervisor.Start], see [daemon.Config]
// for what they mean.
type StartOptions struct {
	Ready   func(mountPoint string) bool
	Timeout time.Duration
	// OnExit is called after the daemon exited, before the repair pass it causes.
	OnExit func(err error)
}

// Start starts cmd as the daemon of volume id, with the scope of the supervisor
// for id, and returns once it serves at mountPoint, see [daemon.Start]. When the
// daemon exits, the supervisor runs a repair pass at once.
func (s *Supervisor) Start(ctx context.Context, id string, cmd *exec.Cmd, mountPoint string, opts StartOptions) (*daemon.Daemon, error) {
	d, err := daemon.Start(ctx, daemon.Config{
		Cmd:        cmd,
		MountPoint: mountPoint,
		Ready:      opts.Ready,
		Timeout:    opts.Timeout,
		Scope:      s.cfg.Scope,
		ID:         id,
		OnExit: func(err error) {
			if opts.OnExit != nil {
				opts.OnExit(err)
			}
			s.loop.Wake()
		},
	})
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.started[id] = d
	s.mu.Unlock()
	return d, nil
}

// PID returns the process id of the daemon of volume id, or 0 when there is none
// or it is not known. A daemon started by this process is known; one of an earlier
// driver is found through its scope, which needs the pids of the host, see
// [scope.Manager.PID].
func (s *Supervisor) PID(id string) int {
	s.mu.Lock()
	d := s.started[id]
	s.mu.Unlock()
	if d != nil {
		select {
		case <-d.Exited():
		default:
			return d.Pid()
		}
	}
	if s.cfg.Scope == nil {
		return 0
	}
	return s.cfg.Scope.PID(id)
}

// ReplaceIn repairs a stale mount inside the daemon of volume id: it replaces
// whatever is mounted at target in the mount namespace of the daemon with a clone
// of the host path source, and leaves the daemon running, see [fusemount.ReplaceIn]
// for the paths and the requirements. It fails, and changes nothing, when the
// volume has no known daemon.
//
// The namespace is opened by the pid of the daemon, and the pid is confirmed
// afterwards: a daemon that exited meanwhile may have had its pid reused by an
// unrelated process, whose namespace would hold the same paths. The daemon is
// confirmed by its scope, or, for a daemon started by this process, by it not
// having been reaped yet.
//
// ReplaceIn does not take the lock of id: call it from Reconcile, which holds it,
// or between [Supervisor.Lock] and [Supervisor.Unlock].
func (s *Supervisor) ReplaceIn(id, source, target string, readOnly bool) error {
	pid := s.PID(id)
	if pid == 0 {
		return fmt.Errorf("replace %s in the daemon of volume %s: no daemon", target, id)
	}
	ns, err := os.Open(fmt.Sprintf("/proc/%d/ns/mnt", pid))
	if err != nil {
		return fmt.Errorf("replace %s in the daemon of volume %s: %w", target, id, err)
	}
	defer ns.Close()
	if !s.isDaemon(id, pid) {
		return fmt.Errorf("replace %s in the daemon of volume %s: process %d is not its daemon any more", target, id, pid)
	}
	return fusemount.ReplaceIn(ns, source, target, readOnly)
}

// isDaemon reports whether pid is the daemon of volume id.
func (s *Supervisor) isDaemon(id string, pid int) bool {
	s.mu.Lock()
	d := s.started[id]
	s.mu.Unlock()
	if d != nil && d.Pid() == pid {
		select {
		case <-d.Exited():
		default:
			return true
		}
	}
	return s.cfg.Scope != nil && s.cfg.Scope.InScope(id, pid)
}
