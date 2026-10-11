package state

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

type volume struct {
	ID     string   `json:"id"`
	Target string   `json:"target"`
	Args   []string `json:"args,omitempty"`
}

// flat is the layout dir/<id>.json, nested dir/<id>/state.json.
func flat(dir string) *Store[volume] {
	return &Store[volume]{
		Path: func(id string) string { return filepath.Join(dir, id+".json") },
		Glob: filepath.Join(dir, "*.json"),
	}
}

func nested(dir string) *Store[volume] {
	return &Store[volume]{
		Path: func(id string) string { return filepath.Join(dir, id, "state.json") },
		Glob: filepath.Join(dir, "*", "state.json"),
	}
}

func TestRoundTripInBothLayouts(t *testing.T) {
	layouts := map[string]func(string) *Store[volume]{"flat": flat, "nested": nested}
	for name, layout := range layouts {
		t.Run(name, func(t *testing.T) {
			s := layout(filepath.Join(t.TempDir(), "state"))

			if all, errs := s.LoadAll(); len(all) != 0 || len(errs) != 0 {
				t.Fatalf("LoadAll on a missing dir = %v, %v; want nothing", all, errs)
			}
			if _, err := s.Load("a"); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("Load of a missing id = %v, want fs.ErrNotExist", err)
			}

			want := volume{ID: "a", Target: "/mnt/a", Args: []string{"-f", "-o", "allow_other"}}
			if err := s.Save("a", want); err != nil {
				t.Fatal(err)
			}
			if err := s.Save("b", volume{ID: "b"}); err != nil {
				t.Fatal(err)
			}
			got, err := s.Load("a")
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("Load = %+v, %v; want %+v", got, err, want)
			}
			all, errs := s.LoadAll()
			if len(errs) != 0 || len(all) != 2 || all[0].ID != "a" || all[1].ID != "b" {
				t.Fatalf("LoadAll = %+v, %v; want a and b", all, errs)
			}

			if err := s.Remove("a"); err != nil {
				t.Fatal(err)
			}
			// Unpublishing is retried, so removing twice must work.
			if err := s.Remove("a"); err != nil {
				t.Fatalf("second Remove: %v", err)
			}
			if all, _ := s.LoadAll(); len(all) != 1 || all[0].ID != "b" {
				t.Fatalf("LoadAll after Remove = %+v, want b", all)
			}
		})
	}
}

func TestSaveReplacesAndLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	s := flat(dir)
	for _, target := range []string{"/one", "/two"} {
		if err := s.Save("a", volume{ID: "a", Target: target}); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := s.Load("a"); got.Target != "/two" {
		t.Errorf("Target = %q, want /two", got.Target)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("state dir holds %d entries, want only a.json", len(entries))
	}
}

// A leftover from a crash during Save must not show up as a volume.
func TestLoadAllIgnoresTempFiles(t *testing.T) {
	dir := t.TempDir()
	s := flat(dir)
	if err := os.WriteFile(filepath.Join(dir, "a.json.tmp"), []byte(`{"id":"a"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if all, errs := s.LoadAll(); len(all) != 0 || len(errs) != 0 {
		t.Fatalf("LoadAll = %v, %v; want nothing", all, errs)
	}
}

// One unreadable state must not keep the others from being repaired.
func TestLoadAllSkipsBadFilesAndReportsThem(t *testing.T) {
	dir := t.TempDir()
	s := flat(dir)
	if err := s.Save("good", volume{ID: "good"}); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{"empty.json": "", "junk.json": "{"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A directory that matches the glob cannot be read as a file.
	if err := os.Mkdir(filepath.Join(dir, "dir.json"), 0o700); err != nil {
		t.Fatal(err)
	}

	all, errs := s.LoadAll()
	if len(all) != 1 || all[0].ID != "good" {
		t.Errorf("LoadAll = %+v, want only good", all)
	}
	if len(errs) != 3 {
		t.Errorf("LoadAll returned %d errors (%v), want 3", len(errs), errs)
	}
}

func TestSaveUsesPrivatePermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	s := flat(dir)
	if err := s.Save("a", volume{ID: "a"}); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]fs.FileMode{dir: 0o700, filepath.Join(dir, "a.json"): 0o600} {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != want {
			t.Errorf("%s mode = %o, want %o", path, got, want)
		}
	}
}
