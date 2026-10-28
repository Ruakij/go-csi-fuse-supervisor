package scope

import (
	"context"
	"errors"
	"fmt"
	"time"

	systemd "github.com/coreos/go-systemd/v22/dbus"
	godbus "github.com/godbus/dbus/v5"
)

// Adopt moves the running process pid into the scope for id, like
// "systemd-run --scope" does for a process it starts itself. It returns once
// systemd has created the scope. The scope is removed when its last process exits,
// so the name can be used again for the next daemon of the volume.
//
// The caller must still hold the process, for example as an unreaped child:
// systemd would otherwise put whatever process owns the pid by then into the scope.
// When ctx ends after systemd queued the scope, Adopt waits a few seconds more
// for the outcome before it returns the error of ctx, so that the caller does not
// release the pid while systemd may still attach it.
//
// Adopt does nothing unless the manager is [Manager.Available].
func (m *Manager) Adopt(ctx context.Context, id string, pid int) error {
	if !m.available {
		return nil
	}
	unit, err := m.Unit(id)
	if err != nil {
		return err
	}
	props := map[string]any{
		"Description": m.cfg.Description,
		"PIDs":        []uint32{uint32(pid)},
		// The daemon may start cgroups of its own.
		"Delegate": true,
		// Removes the unit once the daemon exits, so the volume can reuse the name.
		"CollectMode": "inactive-or-failed",
	}
	if m.cfg.MemoryMax > 0 {
		props["MemoryMax"] = m.cfg.MemoryMax
	}
	if m.cfg.TasksMax > 0 {
		props["TasksMax"] = m.cfg.TasksMax
	}
	list := make([]systemd.Property, 0, len(props))
	for k, v := range props {
		list = append(list, systemd.Property{Name: k, Value: godbus.MakeVariant(v)})
	}

	conn, err := systemd.NewSystemdConnectionContext(ctx)
	if err != nil {
		return fmt.Errorf("connect to host systemd: %w", err)
	}
	defer conn.Close()

	done := make(chan string, 1)
	if _, err := conn.StartTransientUnitContext(ctx, unit, "replace", list, done); err != nil {
		return fmt.Errorf("start scope %s: %w", unit, err)
	}
	select {
	case result := <-done:
		if result != "done" {
			return fmt.Errorf("start scope %s: %s", unit, result)
		}
		return nil
	case <-ctx.Done():
		select {
		case <-done:
		case <-time.After(adoptSettle):
		}
		return ctx.Err()
	}
}

// adoptSettle is how long Adopt waits for systemd to finish a scope it queued
// after the context ended.
const adoptSettle = 5 * time.Second

// Stop asks systemd to stop the scope for id, which kills what is left in it, and
// returns when the stop is done, so that a daemon for the same id can be adopted
// right after. A scope that is not there is not an error; usually the daemon
// has exited and the scope is gone already. If ctx ends first, Stop returns its error.
//
// Stop does nothing unless the manager is [Manager.Available].
func (m *Manager) Stop(ctx context.Context, id string) error {
	if !m.available {
		return nil
	}
	unit, err := m.Unit(id)
	if err != nil {
		return err
	}
	conn, err := systemd.NewSystemdConnectionContext(ctx)
	if err != nil {
		return fmt.Errorf("connect to host systemd to stop %s: %w", unit, err)
	}
	defer conn.Close()
	done := make(chan string, 1)
	if _, err := conn.StopUnitContext(ctx, unit, "replace", done); err != nil {
		var dbusErr godbus.Error
		if errors.As(err, &dbusErr) && dbusErr.Name == "org.freedesktop.systemd1.NoSuchUnit" {
			return nil
		}
		return fmt.Errorf("stop scope %s: %w", unit, err)
	}
	select {
	case result := <-done:
		if result != "done" {
			return fmt.Errorf("stop scope %s: %s", unit, result)
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("wait for scope %s to stop: %w", unit, ctx.Err())
	}
}

// Watch calls onStop with the name of a scope of this manager when it leaves the
// active state, until ctx is done. Daemons started by an earlier driver instance
// are not children of this one, so only systemd sees them exit.
//
// When systemd events were lost because the driver did not keep up, onStop is
// called with an empty name: any scope may have stopped. onStop runs on one
// goroutine of the watch and should return quickly.
//
// Watch returns an error if it cannot subscribe to systemd, and does nothing
// unless the manager is [Manager.Available]. If the D-Bus connection to systemd
// drops later, Watch delivers nothing more and does not reconnect; the interval
// of the reconcile loop is then what finds the scopes that stopped.
func (m *Manager) Watch(ctx context.Context, onStop func(unit string)) error {
	if !m.available {
		return nil
	}
	conn, err := systemd.NewSystemdConnectionContext(ctx)
	if err != nil {
		return err
	}
	updates := make(chan *systemd.PropertiesUpdate, 64)
	dropped := make(chan error, 1)
	conn.SetPropertiesSubscriber(updates, dropped)
	if err := conn.Subscribe(); err != nil {
		conn.Close()
		return err
	}

	go func() {
		defer conn.Close()
		for {
			select {
			case u := <-updates:
				if state, ok := u.Changed["ActiveState"]; ok && m.owns(u.UnitName) && state.Value() != "active" {
					onStop(u.UnitName)
				}
			// The error only says events were lost, which onStop("") covers.
			case <-dropped: //nolint:errcheck
				onStop("")
			case <-ctx.Done():
				return
			}
		}
	}()
	return nil
}
