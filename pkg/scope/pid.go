package scope

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// PID returns the lowest pid of the processes in the scope for id, or 0 when
// there is none, or when [Config.Unit] gives no valid name for id. It looks the
// processes up in /proc, so it finds the daemons of an earlier driver instance
// only when the driver sees the pids of the host.
func (m *Manager) PID(id string) int {
	unit, err := m.Unit(id)
	if err != nil {
		return 0
	}
	return pidIn("/proc", unit)
}

// InScope reports whether the process pid is in the scope for id, by its cgroup
// in /proc. It confirms a pid found earlier, for example by [Manager.PID], which
// another process may have taken over since.
func (m *Manager) InScope(id string, pid int) bool {
	unit, err := m.Unit(id)
	return err == nil && inScopeIn("/proc", unit, pid)
}

// inScopeIn reads the cgroup file of pid. Reading it cannot block on a wedged
// FUSE mount, unlike anything that walks into the mounts of the daemon.
func inScopeIn(procRoot, unit string, pid int) bool {
	b, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "cgroup"))
	return err == nil && inUnit(string(b), unit)
}

func pidIn(procRoot, unit string) int {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return 0
	}
	best := 0
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 || (best != 0 && pid >= best) {
			continue
		}
		if inScopeIn(procRoot, unit, pid) {
			best = pid
		}
	}
	return best
}

// inUnit reports whether a cgroup file has a path with unit as one of its
// components. Matching the component, not the text, keeps "a-b.scope" from
// matching a process in "a-a-b.scope".
func inUnit(cgroupFile, unit string) bool {
	for line := range strings.SplitSeq(cgroupFile, "\n") {
		// hierarchy-id:controllers:path, and the path may contain colons.
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		for c := range strings.SplitSeq(parts[2], "/") {
			if c == unit {
				return true
			}
		}
	}
	return false
}
