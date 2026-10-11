// Package daemon starts a FUSE daemon that has to outlive the CSI driver.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/Ruakij/go-csi-fuse-supervisor/pkg/fusemount"
	"github.com/Ruakij/go-csi-fuse-supervisor/pkg/scope"
)

// Config describes the daemon to start.
type Config struct {
	// Cmd is the daemon, ready to start, and not bound to a context that ends
	// with a request ([exec.CommandContext] would kill the daemon with it). Start sets Setsid on it, so the daemon
	// has no controlling terminal and is not in the process group of the driver,
	// which a signal to the driver would reach.
	//
	// Its stdin, stdout and stderr are used as they are, so they belong to the
	// daemon and not to the driver. Neither of the obvious choices works for a
	// daemon that outlives the driver. A pipe to the driver ends when the driver
	// restarts, and the next write of the daemon fails with EPIPE or kills it with
	// SIGPIPE. The stderr of the driver is held open by the daemon, and a container
	// runtime reports a container as exited only after every holder of its stdio
	// closed it, so the pod of the driver would not terminate. Left nil, the
	// streams are /dev/null; a log file is the way to keep the output.
	Cmd *exec.Cmd
	// MountPoint is where the daemon mounts.
	MountPoint string
	// Ready reports whether the daemon is serving at MountPoint. Default: a live
	// FUSE mount that is not the one that was at MountPoint before Start, which
	// is told by its device, so that a mount left there does not count. A daemon
	// that mounts something other than a FUSE file system, or does more before it
	// serves, supplies its own check.
	Ready func(mountPoint string) bool
	// Timeout is how long the daemon has to become ready. Zero or less: no limit
	// other than the context.
	Timeout time.Duration
	// Scope, if set, moves the daemon into the scope for ID. Without it, or when
	// the manager is not [scope.Manager.Available], the daemon stays in the
	// cgroup of the driver and dies with its pod.
	Scope *scope.Manager
	// ID names the scope. Required with Scope.
	ID string
	// OnExit is called with the result of waiting for the process once it exited
	// after Start returned successfully. It runs on its own goroutine, and the
	// daemon may exit at any time after Start returned, including long after the
	// context passed to it is done. It is not called for a daemon that Start gave up on.
	OnExit func(err error)
}

// Daemon is a running daemon process.
type Daemon struct {
	pid    int
	exited chan struct{}
}

// Pid returns the process id of the daemon.
func (d *Daemon) Pid() int { return d.pid }

// Exited is closed when the daemon has exited and was reaped.
func (d *Daemon) Exited() <-chan struct{} { return d.exited }

// Start starts the daemon and returns once it is ready: the process is in its
// scope, if there is one, and Ready reports true for the mount point. When it
// cannot get there, because the process failed to start or to move into the
// scope, exited, did not become ready within the timeout or the context is done,
// Start kills the process and returns an error. The process may still be
// exiting when that happens.
//
// The context belongs to the call. The daemon is not tied to it and keeps
// running after it is done; stopping it later is up to the caller, see the
// README for when to do that.
func Start(ctx context.Context, cfg Config) (*Daemon, error) {
	switch {
	case cfg.Cmd == nil:
		return nil, errors.New("no command")
	case cfg.Cmd.Process != nil:
		return nil, errors.New("the command was started already")
	case cfg.MountPoint == "":
		return nil, errors.New("no mount point")
	case cfg.Scope != nil && cfg.ID == "":
		return nil, errors.New("a scope needs an id")
	}

	ready := cfg.Ready
	if ready == nil {
		ready = newMountReady(cfg.MountPoint, fusemount.IsFUSE)
	}

	cmd := cfg.Cmd
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", cmd.Path, err)
	}

	d := &Daemon{pid: cmd.Process.Pid, exited: make(chan struct{})}
	var waitErr error
	succeeded := make(chan bool, 1)
	// Reaped only after the process is in its scope: until then a daemon that
	// already died keeps its pid, which the scope would otherwise take from
	// whatever process reuses it.
	adopted := make(chan struct{})
	go func() {
		<-adopted
		waitErr = cmd.Wait()
		close(d.exited)
		if <-succeeded && cfg.OnExit != nil {
			cfg.OnExit(waitErr)
		}
	}()
	giveUp := func(err error) (*Daemon, error) {
		// The daemon leads a session and process group of its own (Setsid), so its
		// children are killed with it.
		_ = syscall.Kill(-d.pid, syscall.SIGKILL)
		succeeded <- false
		return nil, err
	}

	if cfg.Scope != nil {
		if err := cfg.Scope.Adopt(ctx, cfg.ID, d.pid); err != nil {
			close(adopted)
			return giveUp(err)
		}
	}
	close(adopted)

	if err := fusemount.WaitMounted(ctx, cfg.MountPoint, d.exited, cfg.Timeout, ready); err != nil {
		select {
		case <-d.exited:
			// Closed after waitErr was set.
			if waitErr != nil {
				err = fmt.Errorf("%w: %w", err, waitErr)
			}
		default:
		}
		return giveUp(err)
	}
	succeeded <- true
	return d, nil
}

// newMountReady returns the default Ready: a live mount, by isFUSE, whose device
// differs from the one at mountPoint now. A live mount that is there already
// would otherwise make a daemon that has not mounted yet look ready.
func newMountReady(mountPoint string, isFUSE func(string) bool) func(string) bool {
	before := statOf(mountPoint)
	return func(path string) bool {
		if !isFUSE(path) {
			return false
		}
		now := statOf(path)
		return now != nil && (before == nil || now.Dev != before.Dev)
	}
}

// statOf returns the stat of path, or nil if there is none.
func statOf(path string) *syscall.Stat_t {
	fi, err := os.Stat(path)
	if err != nil {
		return nil
	}
	st, _ := fi.Sys().(*syscall.Stat_t)
	return st
}
