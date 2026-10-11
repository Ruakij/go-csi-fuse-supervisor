package reconcile

import "sync"

// Locks is a set of mutexes, one per id. CSI calls and repair passes both change
// a volume, and holding its lock keeps a remount from resurrecting a volume that
// is being unpublished.
//
// Ids are compared exactly, not hashed into a fixed number of buckets, so two
// different ids never wait for each other. The zero value is ready to use.
type Locks struct {
	mu sync.Mutex
	m  map[string]*lock
}

type lock struct {
	mu sync.Mutex
	// refs counts the goroutines that hold the lock or wait for it, and the
	// entry is dropped when it reaches zero, so the map never outgrows the
	// number of volumes in use.
	refs int
}

// Lock blocks until it holds the lock of id.
func (l *Locks) Lock(id string) {
	l.mu.Lock()
	if l.m == nil {
		l.m = map[string]*lock{}
	}
	e := l.m[id]
	if e == nil {
		e = &lock{}
		l.m[id] = e
	}
	e.refs++
	l.mu.Unlock()
	e.mu.Lock()
}

// Unlock releases the lock of id. It must be called by the holder, once per
// Lock: the lock does not know who holds it, so an Unlock by anyone else
// releases it under the holder. It panics if no one holds or waits for the lock.
func (l *Locks) Unlock(id string) {
	l.mu.Lock()
	e := l.m[id]
	if e == nil {
		l.mu.Unlock()
		panic("reconcile: Unlock of unlocked id " + id)
	}
	if e.refs--; e.refs == 0 {
		delete(l.m, id)
	}
	l.mu.Unlock()
	e.mu.Unlock()
}
