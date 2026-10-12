// Package session tracks per-session file-read state and serializes
// operations on a per-path basis (ARCHITECTURE §7).
//
// A "session" is the process lifetime: each MCP client connection maps to
// one container, so process-local state is session state. MCP handlers may
// be invoked concurrently; all shared state flows through this package's
// lock, and every exported method is safe for concurrent use (verified with
// -race).
package session

import (
	"context"
	"sync"
	"time"
)

// Marker is the snapshot of a file taken when it was last successfully read
// (a successful write also records a marker: the content is then known,
// ARCHITECTURE §5.2). Size and ModTime are the only identity signals —
// inodes are deliberately excluded because CIFS inodes are unreliable
// across renames on the target share (§0).
type Marker struct {
	Size    int64
	ModTime time.Time
}

// Status is the outcome of comparing a file's current state against the
// session's recorded marker.
type Status int

const (
	// StatusUnknown means the path has not been read in this session.
	StatusUnknown Status = iota
	// StatusMatch means size and mtime equal the recorded marker.
	StatusMatch
	// StatusStale means size or mtime differ from the recorded marker.
	StatusStale
)

// String names the status for logs and test failures.
func (s Status) String() string {
	switch s {
	case StatusMatch:
		return "match"
	case StatusStale:
		return "stale"
	default:
		return "unknown"
	}
}

// Session holds the read markers plus one mutex per resolved path.
//
// Both maps are guarded by mu for their whole lifetime. The zero value is
// ready to use; New is provided for readers who prefer explicit
// construction.
type Session struct {
	mu      sync.Mutex
	markers map[string]Marker
	// locks serializes same-path operations (§7). A PathLocks table is
	// safe on its own; Session wraps it only so LockFile keeps working on
	// a Session value.
	locks PathLocks
}

// New returns an empty Session.
func New() *Session {
	return &Session{
		markers: make(map[string]Marker),
	}
}

// PathLocks is a per-path mutual-exclusion table with a cancellable
// acquire (§7). Each path gets a cap-1 channel used as a binary semaphore:
// EMPTY means free — acquiring SENDS one token, releasing RECEIVES it
// back. LockContext races the send against ctx.Done(), so a waiter stuck
// behind a long holder (an upload's compare/publish section, say) gives up
// when its budget expires instead of blocking past the deadline.
//
// Entries are never deleted (a deleted entry would let two concurrent
// users of the same path hold two different locks); the set of distinct
// paths per process is small, so retention is cheap.
//
// A PathLocks zero value is ready to use; safe for concurrent use by any
// number of goroutines. The lock is NOT reentrant: never acquire the same
// path twice from one goroutine.
type PathLocks struct {
	mu    sync.Mutex
	locks map[string]chan struct{}
}

// Lock acquires the lock for path and returns the release func. It waits
// as long as the holder keeps the path; use LockContext when the wait must
// stay inside a deadline. The returned func must be called exactly once.
func (t *PathLocks) Lock(path string) func() {
	release, err := t.LockContext(context.Background(), path)
	if err != nil {
		// context.Background is never canceled; unreachable.
		panic("session: Lock on Background context failed: " + err.Error())
	}
	return release
}

// LockContext acquires the lock for path unless ctx finishes first, in
// which case it returns ctx.Err() and holds nothing.
func (t *PathLocks) LockContext(ctx context.Context, path string) (func(), error) {
	ch := t.channel(path)
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// channel returns (creating on first use) the path's semaphore channel.
// A fresh channel is EMPTY, which is the "free" state.
func (t *PathLocks) channel(path string) chan struct{} {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.locks == nil {
		t.locks = make(map[string]chan struct{})
	}
	ch, ok := t.locks[path]
	if !ok {
		ch = make(chan struct{}, 1)
		t.locks[path] = ch
	}
	return ch
}

// MarkRead records path as read at the given size and mtime, overwriting
// any earlier marker for the same path.
func (s *Session) MarkRead(path string, size int64, mtime time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.markers == nil {
		s.markers = make(map[string]Marker)
	}
	s.markers[path] = Marker{Size: size, ModTime: mtime}
}

// Lookup returns the recorded marker for path and whether one exists.
func (s *Session) Lookup(path string) (Marker, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.markers[path]
	return m, ok
}

// Compare classifies the file's current size and mtime against the marker
// recorded for path: StatusUnknown if it was never read, StatusMatch if
// both fields are equal, StatusStale otherwise. mtime comparison uses
// Time.Equal so wall-clock equality is not defeated by location metadata.
func (s *Session) Compare(path string, size int64, mtime time.Time) Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.markers[path]
	if !ok {
		return StatusUnknown
	}
	if m.Size == size && m.ModTime.Equal(mtime) {
		return StatusMatch
	}
	return StatusStale
}

// LockFile acquires the per-path mutex and returns the matching unlock
// function, so callers get a critical section scoped to one resolved path
// (same-path write serialization, §7) while different paths proceed in
// parallel.
//
// The returned func must be called exactly once. The mutex is not
// reentrant: do not call LockFile for the same path again before unlocking.
// LockFile waits without a deadline; new callers that need a bounded wait
// (upload's post-download section) use PathLocks.LockContext directly.
func (s *Session) LockFile(path string) func() {
	return s.locks.Lock(path)
}

// Forget drops the read marker for path, so the next modifying call on it
// is rejected with the never-read EStaleRead variant until it is read
// again.
//
// The per-path lock entry is deliberately retained (see PathLocks):
// deleting a lock that another goroutine still holds would let the next
// acquire create a fresh one and bypass serialization.
func (s *Session) Forget(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.markers, path)
}
