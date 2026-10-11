// Package state keeps one JSON file of state per id, written atomically, so a
// restarted driver can repair what an earlier one left running.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Store reads and writes the state files of one kind of object. The layout is the
// caller's: Path and Glob together decide whether the files are dir/<id>.json,
// dir/<hash>/state.json or anything else.
//
// A Store holds no state of its own, and does not lock: concurrent calls for the
// same id need the caller's per-id lock (see reconcile.Locks).
type Store[T any] struct {
	// Path returns the file that holds the state of id. The name must be safe to
	// use as a path: ids may contain any character, so a caller sanitizes or
	// hashes them here.
	Path func(id string) string
	// Glob matches the file of every id, and nothing else; the temporary files
	// of a write must not match it, which "dir/*.json" and "dir/*/state.json"
	// both ensure.
	Glob string
}

// Save writes v as the state of id. The file is written to a temporary name,
// synced and renamed into place, so neither a crash nor a concurrent reader sees
// a half-written file. The parent directory is created if missing.
func (s *Store[T]) Save(id string, v T) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal state of %s: %w", id, err)
	}
	final := s.Path(id)
	if err := os.MkdirAll(filepath.Dir(final), 0o700); err != nil {
		return fmt.Errorf("create state dir of %s: %w", id, err)
	}
	tmp := final + ".tmp"
	if err := writeSynced(tmp, data); err != nil {
		return fmt.Errorf("write state of %s: %w", id, err)
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("write state of %s: %w", id, err)
	}
	return nil
}

// Load returns the state of id. A missing file is an error satisfying
// errors.Is(err, fs.ErrNotExist).
func (s *Store[T]) Load(id string) (T, error) {
	return read[T](s.Path(id))
}

// Remove deletes the state of id. Removing state that does not exist is not an
// error, as unpublishing is retried.
func (s *Store[T]) Remove(id string) error {
	if err := os.Remove(s.Path(id)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove state of %s: %w", id, err)
	}
	return nil
}

// LoadAll returns every state that matches Glob, in file name order. A file that
// cannot be read or decoded is skipped and its error returned in the second
// result, so one bad file never keeps the others from being repaired. A file
// removed meanwhile is skipped without an error.
func (s *Store[T]) LoadAll() ([]T, []error) {
	files, err := filepath.Glob(s.Glob)
	if err != nil {
		return nil, []error{fmt.Errorf("list state files: %w", err)}
	}
	var all []T
	var errs []error
	for _, file := range files {
		v, err := read[T](file)
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			errs = append(errs, err)
		default:
			all = append(all, v)
		}
	}
	return all, errs
}

func read[T any](file string) (T, error) {
	var v T
	data, err := os.ReadFile(file)
	if err != nil {
		return v, fmt.Errorf("read state: %w", err)
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return v, fmt.Errorf("decode state %s: %w", file, err)
	}
	return v, nil
}

func writeSynced(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}
