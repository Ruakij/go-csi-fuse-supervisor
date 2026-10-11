// Package scope moves FUSE daemons into transient systemd scopes of the host.
//
// A daemon in a scope belongs to the service manager of the host, not to the
// cgroup of the container that started it, so the container runtime does not kill
// it with the CSI driver pod. A scope is named after a volume id, which makes the
// daemon of a volume findable and stoppable after the driver restarted, without
// any record of the process.
package scope

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	systemd "github.com/coreos/go-systemd/v22/dbus"
)

// Lifetime selects where daemons live.
type Lifetime int

const (
	// Auto uses host systemd when it is reachable and the container otherwise.
	Auto Lifetime = iota
	// Systemd requires host systemd; [New] fails when it is unreachable.
	Systemd
	// InContainer leaves daemons in the cgroup of the process that started them.
	InContainer
)

var lifetimeNames = [...]string{Auto: "auto", Systemd: "systemd", InContainer: "in-container"}

// String returns the name [ParseLifetime] accepts.
func (l Lifetime) String() string {
	if l < 0 || int(l) >= len(lifetimeNames) {
		return fmt.Sprintf("Lifetime(%d)", int(l))
	}
	return lifetimeNames[l]
}

// ParseLifetime parses "auto", "systemd" or "in-container", the values of a
// driver flag.
func ParseLifetime(s string) (Lifetime, error) {
	for l, name := range lifetimeNames {
		if s == name {
			return Lifetime(l), nil
		}
	}
	return Auto, fmt.Errorf("unknown daemon lifetime %q, want auto, systemd or in-container", s)
}

// Config configures a [Manager].
type Config struct {
	// Prefix starts the name of every scope and tells the scopes of the driver
	// from all others on the host, so it is unique to the driver, for example
	// "csi-rclone-". Required; letters, digits and "-_." only, at most 64 characters.
	Prefix string
	// Description is shown by systemctl. Default: "FUSE daemon of <Prefix>".
	Description string
	// Lifetime defaults to [Auto].
	Lifetime Lifetime
	// MemoryMax and TasksMax limit each scope; zero means no limit.
	MemoryMax, TasksMax uint64
	// Unit names the scope of a volume id, for a driver whose scopes already have
	// a naming that has to stay. Default: see [Manager.Unit]. The name must
	// start with Prefix and end in ".scope", or [Manager.Watch] would not see the
	// scope; the methods that use it return an error for any other. It must be
	// derived from the id alone, as the scope is found again by it after a restart
	// of the driver.
	Unit func(id string) string
}

// Manager moves processes into scopes named after the volumes they serve. It is
// safe for concurrent use.
type Manager struct {
	cfg       Config
	available bool
	fellBack  error
}

const (
	scopeSuffix = ".scope"
	maxPrefix   = 64
	// An id that is longer is shortened and hashed. Together with the prefix the
	// name stays below the 255 characters systemd and the cgroup file system allow.
	maxIDLen   = 100
	hashedKeep = 40
	hashLen    = 16
)

// New returns a Manager. Unless the lifetime is [InContainer] it connects to host
// systemd once to find out whether scopes can be used; for [Systemd] a failure is
// an error, for [Auto] it makes [Manager.Available] false and [Manager.FellBack]
// return the reason.
func New(ctx context.Context, cfg Config) (*Manager, error) {
	return newManager(ctx, cfg, probe)
}

func newManager(ctx context.Context, cfg Config, probe func(context.Context) error) (*Manager, error) {
	if err := validPrefix(cfg.Prefix); err != nil {
		return nil, err
	}
	if cfg.Lifetime < Auto || cfg.Lifetime > InContainer {
		return nil, fmt.Errorf("invalid daemon lifetime %d", cfg.Lifetime)
	}
	if cfg.Description == "" {
		cfg.Description = "FUSE daemon of " + cfg.Prefix
	}
	m := &Manager{cfg: cfg}
	if cfg.Lifetime == InContainer {
		return m, nil
	}
	if err := probe(ctx); err != nil {
		if cfg.Lifetime == Systemd {
			return nil, fmt.Errorf("host systemd is unreachable: %w", err)
		}
		m.fellBack = err
		return m, nil
	}
	m.available = true
	return m, nil
}

func probe(ctx context.Context) error {
	conn, err := systemd.NewSystemdConnectionContext(ctx)
	if err != nil {
		return err
	}
	conn.Close()
	return nil
}

func validPrefix(p string) error {
	if p == "" {
		return errors.New("the scope prefix is empty")
	}
	if len(p) > maxPrefix || sanitize(p) != p {
		return fmt.Errorf("scope prefix %q: want at most %d characters from letters, digits, dash, underscore and dot", p, maxPrefix)
	}
	return nil
}

// Available reports whether daemons are moved into scopes. When it is false,
// [Manager.Adopt], [Manager.Stop] and [Manager.Watch] do nothing.
func (m *Manager) Available() bool { return m.available }

// FellBack returns why [Auto] chose to leave daemons in the container, and nil
// when scopes are used or the lifetime is not [Auto].
func (m *Manager) FellBack() error { return m.fellBack }

// Unit returns the name of the scope for id. It is derived from id alone, so
// it is the same after a restart of the driver. An id of letters, digits and
// "-_." is used as it is; any other is sanitized and gets a hash of the original
// appended, so different ids do not share a scope.
//
// With [Config.Unit] the name is the one it returns, and an error if that does
// not start with the prefix and end in ".scope".
func (m *Manager) Unit(id string) (string, error) {
	if m.cfg.Unit == nil {
		return m.defaultUnit(id), nil
	}
	unit := m.cfg.Unit(id)
	if !m.owns(unit) {
		return "", fmt.Errorf("scope name %q for volume %q: want one that starts with %q and ends in %q", unit, id, m.cfg.Prefix, scopeSuffix)
	}
	return unit, nil
}

func (m *Manager) defaultUnit(id string) string {
	name := id
	if clean := sanitize(id); clean != id || len(id) > maxIDLen || id == "" {
		sum := sha256.Sum256([]byte(id))
		if len(clean) > hashedKeep {
			clean = clean[:hashedKeep]
		}
		name = clean + "-" + hex.EncodeToString(sum[:])[:hashLen]
	}
	return m.cfg.Prefix + name + scopeSuffix
}

// owns reports whether unit is a scope of this manager.
func (m *Manager) owns(unit string) bool {
	return strings.HasPrefix(unit, m.cfg.Prefix) && strings.HasSuffix(unit, scopeSuffix)
}

// sanitize replaces every character that is not a letter, digit or one of "-_."
// with "-". The unit name reaches the service manager of the host, so ids are
// not trusted to be tame.
func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}
