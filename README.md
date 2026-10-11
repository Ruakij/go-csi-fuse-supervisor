# go-csi-fuse-supervisor

[![Linux](https://img.shields.io/badge/Linux-5.12%2B-FCC624?logo=linux&logoColor=black)](#requirements)
[![Go Reference](https://pkg.go.dev/badge/github.com/Ruakij/go-csi-fuse-supervisor.svg)](https://pkg.go.dev/github.com/Ruakij/go-csi-fuse-supervisor)

Building blocks for CSI drivers whose volumes are served by FUSE daemons that
have to outlive the driver: start a daemon in a systemd scope of the host, tell
a live mount from a dead one, and repair what died or went stale, without
touching what still works.

```go
sup, _ := supervisor.New(supervisor.Config{
	Scope:     scopes,       // optional: daemons live in scopes of host systemd
	IDs:       volumeIDs,    // the volumes with a daemon to look after
	Reconcile: reconcile,    // checks and repairs one volume, under its lock
})
go sup.Run(ctx)              // a pass at start, then every 30s and on every daemon exit

d, err := sup.Start(ctx, id, cmd, mountPoint, supervisor.StartOptions{})
```

## Why

A FUSE mount lives as long as its daemon. When the daemon runs in the pod of the
CSI driver, an upgrade or eviction of that pod takes every volume down with it,
and kubelet never remounts a volume for a pod that is already running: the pod
reads `ENOTCONN` until it restarts. The daemon therefore has to outlive the
driver pod, and the next driver instance has to find it again, judge whether it
still serves, and repair the volumes that lost theirs.

The mount-and-repair machinery is the same in every such driver: scopes with
stable names, the `statfs` check that tells live, dead and gone apart, bind
mounts that keep the read-only attribute, a reconcile loop with locks shared with
the CSI calls. It was written twice, in union-csi-driver and csi-rclone, and
drifted. This module holds the best variant of each part.

## Requirements

- Linux 5.12 or newer (`mount_setattr`), run as root. Other systems build, so
  that callers can vet and test on them, but the mount functions return an error.
- Host systemd, reached through its D-Bus socket (`/run/systemd`), for scopes.
  Without it, [`scope.Auto`](#pkgscope) leaves daemons in the container.
- The pids of the host (`hostPID`) to find the daemon of a volume that an earlier
  driver started (`scope.Manager.PID`) and to work in its mount namespace
  (`fusemount.ReplaceIn`).

## Packages

All packages are usable on their own; `supervisor` only joins them.

### pkg/fusemount

Looking at and changing mounts.

- `Status(path) State` classifies a path by `statfs`: `Live` (a FUSE file
  system), `Dead` (`ENOTCONN`: the daemon is gone), `Gone` (`ENOENT`) or `Other`.
  `IsFUSE(path)` is `Status(path) == Live`.
- `Prober` is `Status` for paths that may hang. A `statfs` on the mount of a
  daemon that is alive but stuck never returns and cannot be cancelled, so
  `Probe` gives up after a timeout with `ErrHung` and does not probe the path
  again until the stuck call returned. `Forget` drops the hung mark of a path,
  for after the mount there was repaired: the stuck call belongs to the old mount
  and would keep the path from being probed.
- `Unmount(path)` unmounts lazily, everything stacked at the path, and is
  idempotent. The path is looked up without walking into the mount, so a dead
  or hung mount does not block it.
- `Bind(source, target, readOnly)` binds a tree with its submounts. A read-only
  bind is cloned and marked read-only before it is attached: a remount
  afterwards would change only the copy of the caller's namespace and not a
  propagated host copy. A read-only bind also gets `nosuid` and `nodev`, so a
  remote tree cannot bring setuid files or device nodes into the container.
- `WaitMounted(ctx, path, exited, timeout, ready)` waits until the daemon serves,
  gives up when it exits or the timeout passes, and is not held up by a check
  that hangs.
- `ReplaceIn(ns, source, target, readOnly)` replaces the mount at `target`
  inside the mount namespace `ns` (an open `/proc/<pid>/ns/mnt`) with a clone of
  the host path `source`, and leaves the process and every other mount alone.
  `target` is an absolute, clean path other than `/`, resolved from the root of
  the mount namespace, not from the root of a process. The mount that holds
  `target` must be private or slave: under a shared mount the clone would
  propagate to its peers. The caller opens `ns` first and confirms afterwards
  that the pid still belongs to the daemon, as `Supervisor.ReplaceIn` does. See
  the repair policy.

### pkg/scope

Transient systemd scopes, like `systemd-run --scope` for a process that is
already running.

- `New(ctx, Config)` builds a `Manager` with a unit name prefix, a description,
  `MemoryMax`, `TasksMax`, and a lifetime: `Auto` (scopes when host systemd is
  reachable, probed once), `Systemd` (an error when it is not) or `InContainer`.
- `Unit(id)` is the scope name for a volume id, and an error when the name is
  not valid. It is derived from the id alone, so a restarted driver finds the
  scope without any record. An id of letters, digits and `-_.` is used as it is,
  any other is sanitized and gets a hash appended. A driver whose scopes already
  have a naming sets `Config.Unit`; its names start with the prefix and end in
  `.scope`, and `Adopt`, `Stop` and the lookups fail for any other.
- `Adopt(ctx, id, pid)` moves a running process into the scope, `Stop(ctx, id)`
  stops it and returns when systemd finished the stop, so a `Start` after it
  finds the name free, `Watch(ctx, onStop)` calls `onStop` when a scope of the
  manager leaves the active state, `PID(id)` finds the lowest pid in a scope by
  its cgroup in `/proc`, and `InScope(id, pid)` confirms that a pid is still in
  the scope. When systemd events were dropped, `onStop` gets an empty name: any
  scope may have stopped. When the D-Bus connection of `Watch` drops, it delivers
  nothing more and does not reconnect; the interval of the reconcile loop is the
  fallback that finds the exit.

### pkg/daemon

`Start(ctx, Config)` starts an `*exec.Cmd` as a daemon in a session of its own,
moves it into its scope, and returns once the mount is ready. A daemon that
exits, does not become ready in time or loses the context is killed. The
process is adopted into the scope before it is reaped: until then a daemon that
already died keeps its pid, which the scope would otherwise take from whatever
process reuses it.

The streams of the command are the caller's. A pipe to the driver breaks when
the driver restarts, and the next write of the daemon fails with `EPIPE` or
kills it with `SIGPIPE`. The stderr of the driver is held open by the daemon, and
a container runtime reports a container as exited only after everyone closed its
stdio, so the pod of the driver would not terminate. Left unset, the streams are
`/dev/null`; a log file keeps the output.

A daemon that is given up on is killed with its process group. Unless
`Config.Ready` is set, the daemon is ready when its mount point is a live FUSE
mount on a different device than before the start, so a mount that was there
already does not count.

`Daemon` has `Pid()` and `Exited()`, a channel that is closed when the process was
reaped. `Config.OnExit` gets the result of the wait. There is no function to stop
a daemon, see the repair policy.

### pkg/state

`Store[T]` keeps one JSON file per volume: `Save` writes a temporary file, syncs
and renames it, so a crash leaves the old or the new file; `Load`, `Remove`, and
`LoadAll`, which returns every file that can be read and the errors of those that
cannot.

Writing the state before the daemon starts leaves a state for a mount that never
came up after a crash between the two, which a reconcile pass repairs. The
reverse order would leave a live mount that nothing knows how to repair.

### pkg/reconcile

`Loop` calls `Pass(ctx, first)` immediately, then every `Interval` (30 seconds)
and whenever `Wake()` asks. Wakes collapse: a burst of them, such as several
daemons exiting together, is one pass. Passes start at least `MinGap` (one
second) apart, so a daemon that dies right after it started does not cause a
tight loop of remounts.

`Locks` is one mutex per id, compared exactly. CSI calls and passes both change a
volume, and holding its lock keeps a remount from resurrecting a volume that is
being unpublished.

### pkg/supervisor

Joins the above. `Run` starts the watch on the scopes, which wakes the loop when
one stops (if the watch fails, the next pass finds the exit), and the loop.
Every pass calls `Reconcile(ctx, id, first)` for each id from `IDs`, with the
lock of the id held; when `IDs` returns ids together with an error, the error
goes to `OnError` and the ids are reconciled. `Lock` and `Unlock` take the same
lock for the CSI calls, and `Unlock` is for the holder only. `Start` starts a
daemon in the scope of its id and wakes the loop when it exits, `PID` returns the
pid of a volume's daemon, and `ReplaceIn(id, source, target, readOnly)` repairs a
mount inside that daemon. It opens the mount namespace by the pid and confirms
that the pid is still the daemon, by its scope or by it not having been reaped,
before it changes anything.

```go
scopes, err := scope.New(ctx, scope.Config{Prefix: "csi-example-", MemoryMax: 1 << 30})
if err != nil {
	log.Fatal(err)
}
if err := scopes.FellBack(); err != nil {
	log.Printf("no host systemd, daemons die with this pod: %v", err)
}

var sup *supervisor.Supervisor
sup, err = supervisor.New(supervisor.Config{
	Scope: scopes,
	IDs: func() ([]string, error) {
		all, errs := volumes.LoadAll() // a state.Store
		ids := make([]string, 0, len(all))
		for _, v := range all {
			ids = append(ids, v.ID)
		}
		return ids, errors.Join(errs...)
	},
	Reconcile: func(ctx context.Context, id string, first bool) {
		v, err := volumes.Load(id) // under the lock: the volume may be gone
		if err != nil {
			return
		}
		if first {
			log.Printf("volume %s was left by an earlier driver", id)
		}
		if fusemount.Status(v.MountPoint) == fusemount.Dead {
			_ = fusemount.Unmount(v.MountPoint)
			if err := start(ctx, sup, v); err != nil { // sup.Start with the command of v
				log.Printf("remount %s: %v", id, err)
			}
		}
	},
	OnError: func(err error) { log.Print(err) },
})
go sup.Run(ctx)
```

A complete version that compiles is in
[`pkg/supervisor/example_test.go`](pkg/supervisor/example_test.go).

## Repair policy

The supervisor decides nothing: `Reconcile` does. These rules hold for
what it should do, and the packages are built around them.

**A daemon that still serves is never stopped or replaced.** Every container that
uses a volume holds its own bind copy of the mount, taken when the container
started, and that copy keeps the superblock of the daemon it was taken from.
Kubelet does not take it again for a running pod. A new mount at the publish path
does not reach those copies: the running pods stay on the old mount, which dies
with its daemon, and see the new one only after a restart. So a daemon that
answers is left alone, even when something around it looks wrong. A daemon whose
mount point hangs is not stopped or replaced either: a hang in the daemon of the
driver is a fault to report, not a death. Drivers probe the mount point of
the daemon through the `Prober` as well, since a hung branch can hang the `statfs`
of the union target that serves it.

**A daemon that died is replaced** at the same path (`Unmount`, `Stop` of what
is left of its scope, then `daemon.Start`; `Stop` returns once the scope is gone,
so the new scope takes its name). The bind mounts of the publish paths point at
the dead mount and are bound again from the new one (`Unmount` of the target,
then `Bind`).
Containers that hold a copy of the dead mount stay on it until they restart.

**A stale part inside a daemon that serves is repaired in place.** A union daemon
in a sandbox sees its branches as binds in a mount namespace of its own. When the
daemon behind a branch dies and is replaced, the bind inside stays on the dead
mount, while the branch is healthy on the host. The driver probes both views of a
branch through the `Prober`: the view of the daemon, `/proc/<pid>/root/<path>`,
and the view of the host. A path that is dead or hung in the view of the daemon
while it is `Live` on the host is stale. It is repaired in place with
`Supervisor.ReplaceIn(id, branch, target, readOnly)` only after several
consecutive passes found it stale, and the path of the daemon view is dropped
from the `Prober` with `Forget` afterwards. A branch that is broken on the host
too is left alone and reported: a clone of it would be as broken. The count of
passes is the driver's; the library has no helper for it.

`ReplaceIn` detaches the stale bind by path, never walking into the dead or hung
mount, and attaches a clone of the host path, in the namespace of the daemon. The
daemon keeps running, and the mount that its pods hold is not touched.

**A daemon is stopped when its volume is removed.** After the unmount `Stop`
removes what is left of its scope, which is usually nothing: the daemon exits on
unmount and the empty scope is collected.

## Development

```sh
make test              # unit tests with the race detector
make test-mount        # mount tests, in a privileged container
make test-mount-systemd  # the same, against the systemd of the Docker host
```

`make test-mount` runs the tests tagged `mounttest` as root in a container: bind
mounts, a real FUSE file system served from the test, `ReplaceIn` into processes
in mount namespaces of their own (also after a `pivot_root`). On a machine that
is not Linux, the container runs in Colima or another VM. `make test-mount-systemd`
hands in the systemd socket and the pids of the host so that the scope tests run
against a real service manager; it needs a Docker host that runs systemd.

The pre-commit hook runs formatting, vet for Linux and macOS, the tests and
golangci-lint on what is staged:

```sh
git config core.hooksPath .githooks
```

## License

Apache-2.0, see [LICENSE](LICENSE).
