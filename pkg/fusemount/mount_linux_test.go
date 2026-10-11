//go:build linux

package fusemount

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStatusOfPlainPaths(t *testing.T) {
	dir := t.TempDir()
	if got := Status(dir); got != Other {
		t.Errorf("Status of a directory = %v, want other", got)
	}
	if got := Status(filepath.Join(dir, "missing")); got != Gone {
		t.Errorf("Status of a missing path = %v, want gone", got)
	}
	if IsFUSE(dir) {
		t.Error("IsFUSE of a directory = true")
	}
}

// Unmount must tolerate being called for something that is not mounted, as
// unpublishing is retried.
func TestUnmountOfNothingIsNotAnError(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("unmounting needs root, make test-mount runs this as root")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{dir, file, link, filepath.Join(dir, "missing")} {
		for range 2 {
			if err := Unmount(path); err != nil {
				t.Errorf("Unmount(%s) = %v, want nil", path, err)
			}
		}
	}
}

// The paths are checked before the namespace is looked at.
func TestReplaceInRefusesBadPaths(t *testing.T) {
	tests := []struct{ source, target, want string }{
		{"rel", "/abs", "absolute"},
		{"/abs", "rel", "absolute"},
		{"/abs", "/", "clean"},
		{"/abs", "/a/", "clean"},
		{"/abs", "/a//b", "clean"},
		{"/abs", "/a/./b", "clean"},
		{"/abs", "/a/../b", "clean"},
	}
	for _, tt := range tests {
		err := ReplaceIn(nil, tt.source, tt.target, false)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("ReplaceIn(%q, %q) = %v, want an error about %s", tt.source, tt.target, err, tt.want)
		}
	}
	if err := ReplaceIn(nil, "/abs", "/a/b", false); err == nil || !strings.Contains(err.Error(), "no mount namespace") {
		t.Errorf("ReplaceIn without a namespace = %v", err)
	}
}
