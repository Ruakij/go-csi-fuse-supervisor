package scope

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTest(t *testing.T, cfg Config, probeErr error) (*Manager, error) {
	t.Helper()
	return newManager(context.Background(), cfg, func(context.Context) error { return probeErr })
}

// mustUnit returns the scope name for id and fails the test when there is none.
func mustUnit(t *testing.T, m *Manager, id string) string {
	t.Helper()
	u, err := m.Unit(id)
	if err != nil {
		t.Fatalf("Unit(%q): %v", id, err)
	}
	return u
}

func TestUnit(t *testing.T) {
	m, err := newTest(t, Config{Prefix: "csi-x-", Lifetime: InContainer}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := mustUnit(t, m, "pvc-123_a.b"), "csi-x-pvc-123_a.b.scope"; got != want {
		t.Errorf("Unit of a plain id = %q, want %q", got, want)
	}

	seen := map[string]string{}
	for _, id := range []string{
		"", "a/b", "a-b", "a b", "a:b", "a\nb", "ä", "ö", "x/" + strings.Repeat("y", 300),
		strings.Repeat("y", maxIDLen), strings.Repeat("y", maxIDLen+1), strings.Repeat("y", maxIDLen+2),
	} {
		u := mustUnit(t, m, id)
		if u != mustUnit(t, m, id) {
			t.Errorf("Unit(%q) is not deterministic", id)
		}
		if len(u) > 255 {
			t.Errorf("Unit(%q) has %d characters", id, len(u))
		}
		if sanitize(strings.TrimSuffix(u, ".scope")) != strings.TrimSuffix(u, ".scope") || !m.owns(u) {
			t.Errorf("Unit(%q) = %q is not a valid unit name of the manager", id, u)
		}
		if prev, ok := seen[u]; ok {
			t.Errorf("%q and %q share the unit %q", prev, id, u)
		}
		seen[u] = id
	}

	// What sanitizing alone would turn into the same name stays apart.
	if mustUnit(t, m, "a/b") == mustUnit(t, m, "a-b") || mustUnit(t, m, "a/b") == mustUnit(t, m, "a:b") {
		t.Error("ids that differ only in invalid characters share a unit")
	}
	if got := mustUnit(t, m, "a/b"); !strings.HasPrefix(got, "csi-x-a-b-") {
		t.Errorf("Unit(a/b) = %q, want the sanitized id before the hash", got)
	}
}

// A driver with scopes of its own naming keeps them, as long as the manager still
// recognizes them by prefix and suffix.
func TestUnitOverride(t *testing.T) {
	m, err := newTest(t, Config{
		Prefix:   "csi-x-",
		Lifetime: InContainer,
		Unit: func(id string) string {
			switch id {
			case "no-prefix":
				return "other-" + id + ".scope"
			case "no-suffix":
				return "csi-x-" + id + ".service"
			case "empty":
				return ""
			}
			return "csi-x-" + strings.ToUpper(id) + ".scope"
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := mustUnit(t, m, "vol-1"), "csi-x-VOL-1.scope"; got != want {
		t.Errorf("Unit = %q, want the name the override gave, %q", got, want)
	}
	for _, id := range []string{"no-prefix", "no-suffix", "empty"} {
		if u, err := m.Unit(id); err == nil {
			t.Errorf("Unit(%q) = %q, want an error for a name the manager would not recognize", id, u)
		}
		if got := m.PID(id); got != 0 {
			t.Errorf("PID(%q) = %d, want 0", id, got)
		}
		if m.InScope(id, os.Getpid()) {
			t.Errorf("InScope(%q) = true", id)
		}
	}
}

// An invalid name is an error of the call that needs it, before it reaches systemd.
func TestInvalidUnitFailsAdoptAndStop(t *testing.T) {
	m, err := newTest(t, Config{Prefix: "csi-x-", Unit: func(string) string { return "wrong.scope" }}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !m.Available() {
		t.Fatal("the manager is not available, the test would not reach the name")
	}
	ctx := context.Background()
	if err := m.Adopt(ctx, "v", 1); err == nil {
		t.Error("Adopt with an invalid name succeeded")
	}
	if err := m.Stop(ctx, "v"); err == nil {
		t.Error("Stop with an invalid name succeeded")
	}
}

func TestOwns(t *testing.T) {
	m, _ := newTest(t, Config{Prefix: "csi-x-"}, nil)
	for unit, want := range map[string]bool{
		"csi-x-vol.scope":        true,
		"csi-x-.scope":           true,
		"csi-x-vol.service":      false,
		"csi-y-vol.scope":        false,
		"cri-containerd-1.scope": false,
		"":                       false,
	} {
		if got := m.owns(unit); got != want {
			t.Errorf("owns(%q) = %v, want %v", unit, got, want)
		}
	}
}

func TestPrefixIsValidated(t *testing.T) {
	for _, p := range []string{"", "a b", "a/", "a:", strings.Repeat("a", maxPrefix+1)} {
		if _, err := newTest(t, Config{Prefix: p}, nil); err == nil {
			t.Errorf("prefix %q accepted", p)
		}
	}
	if _, err := newTest(t, Config{Prefix: "ok-", Lifetime: Lifetime(7)}, nil); err == nil {
		t.Error("invalid lifetime accepted")
	}
}

func TestLifetimeModes(t *testing.T) {
	boom := errors.New("no bus")
	tests := []struct {
		name      string
		lifetime  Lifetime
		probeErr  error
		wantErr   bool
		available bool
		fellBack  bool
	}{
		{"auto with systemd", Auto, nil, false, true, false},
		{"auto without systemd falls back", Auto, boom, false, false, true},
		{"systemd with systemd", Systemd, nil, false, true, false},
		{"systemd without systemd fails", Systemd, boom, true, false, false},
		{"in-container never probes", InContainer, boom, false, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			probed := false
			m, err := newManager(context.Background(), Config{Prefix: "p-", Lifetime: tt.lifetime},
				func(context.Context) error { probed = true; return tt.probeErr })
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				if !errors.Is(err, boom) {
					t.Errorf("err = %v, want the probe error wrapped", err)
				}
				return
			}
			if m.Available() != tt.available {
				t.Errorf("Available = %v, want %v", m.Available(), tt.available)
			}
			if (m.FellBack() != nil) != tt.fellBack {
				t.Errorf("FellBack = %v, want fellBack %v", m.FellBack(), tt.fellBack)
			}
			if tt.lifetime == InContainer && probed {
				t.Error("probed host systemd")
			}
		})
	}
}

// Without scopes the methods are no-ops that never touch D-Bus, which is how a
// driver in a container without host systemd runs.
func TestUnavailableManagerDoesNothing(t *testing.T) {
	m, _ := newTest(t, Config{Prefix: "p-", Lifetime: InContainer}, nil)
	ctx := context.Background()
	if err := m.Adopt(ctx, "v", 1); err != nil {
		t.Errorf("Adopt: %v", err)
	}
	if err := m.Stop(ctx, "v"); err != nil {
		t.Errorf("Stop: %v", err)
	}
	if err := m.Watch(ctx, func(string) { t.Error("onStop called") }); err != nil {
		t.Errorf("Watch: %v", err)
	}
}

func TestDescriptionDefault(t *testing.T) {
	m, _ := newTest(t, Config{Prefix: "p-", Lifetime: InContainer}, nil)
	if m.cfg.Description != "FUSE daemon of p-" {
		t.Errorf("Description = %q", m.cfg.Description)
	}
}

func TestParseLifetime(t *testing.T) {
	for _, l := range []Lifetime{Auto, Systemd, InContainer} {
		got, err := ParseLifetime(l.String())
		if err != nil || got != l {
			t.Errorf("ParseLifetime(%q) = %v, %v", l.String(), got, err)
		}
	}
	if _, err := ParseLifetime("Auto"); err == nil {
		t.Error("ParseLifetime accepted an unknown name")
	}
	if got := Lifetime(9).String(); got != "Lifetime(9)" {
		t.Errorf("String of an invalid lifetime = %q", got)
	}
}

func writeCgroup(t *testing.T, root, pid, content string) {
	t.Helper()
	dir := filepath.Join(root, pid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPIDScansCgroups(t *testing.T) {
	root := t.TempDir()
	const unit = "a-b.scope"
	writeCgroup(t, root, "5", "0::/system.slice/a-a-b.scope\n")          // contains the name as text only
	writeCgroup(t, root, "100", "0::/system.slice/a-b.scope\n")          // the daemon
	writeCgroup(t, root, "20", "0::/system.slice/a-b.scope/delegated\n") // a helper in a cgroup of the daemon
	writeCgroup(t, root, "9", "0::/system.slice/other.scope\n")
	writeCgroup(t, root, "7", "12:memory:/x\n1:name=systemd:/system.slice/a-b.scope\n") // v1 line with controllers
	writeCgroup(t, root, "3", "0::/../../system.slice/a-b.scope-not\n")
	writeCgroup(t, root, "self", "0::/system.slice/a-b.scope\n")
	if err := os.MkdirAll(filepath.Join(root, "8"), 0o755); err != nil { // no cgroup file
		t.Fatal(err)
	}

	if got := pidIn(root, unit); got != 7 {
		t.Errorf("pidIn = %d, want the lowest pid in the unit, 7", got)
	}
	if got := pidIn(root, "none.scope"); got != 0 {
		t.Errorf("pidIn of an unknown unit = %d, want 0", got)
	}
	if got := pidIn(filepath.Join(root, "missing"), unit); got != 0 {
		t.Errorf("pidIn of a missing /proc = %d, want 0", got)
	}
}

func TestInScope(t *testing.T) {
	root := t.TempDir()
	writeCgroup(t, root, "100", "0::/system.slice/a-b.scope\n")
	writeCgroup(t, root, "101", "0::/system.slice/a-a-b.scope\n")
	writeCgroup(t, root, "102", "0::/system.slice/a-b.scope/delegated\n")
	for pid, want := range map[int]bool{100: true, 101: false, 102: true, 103: false, 0: false, -1: false} {
		if got := inScopeIn(root, "a-b.scope", pid); got != want {
			t.Errorf("inScopeIn(%d) = %v, want %v", pid, got, want)
		}
	}
}

func TestPIDUsesTheUnitOfTheID(t *testing.T) {
	m, _ := newTest(t, Config{Prefix: "p-", Lifetime: InContainer}, nil)
	if got := m.PID("no-such-volume-for-sure"); got != 0 {
		t.Errorf("PID = %d, want 0", got)
	}
}
