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
	locks   map[string]*sync.Mutex
}

// New returns an empty Session.
func New() *Session {
	return &Session{
		markers: make(map[string]Marker),
		locks:   make(map[string]*sync.Mutex),
	}
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
func (s *Session) LockFile(path string) func() {
	s.mu.Lock()
	if s.locks == nil {
		s.locks = make(map[string]*sync.Mutex)
	}
	mu := s.locks[path]
	if mu == nil {
		mu = &sync.Mutex{}
		s.locks[path] = mu
	}
	s.mu.Unlock()

	mu.Lock()
	return mu.Unlock
}

// Forget drops the read marker for path, so the next write to it is
// rejected as unread until it is read again.
//
// The per-path mutex entry is deliberately retained: deleting a mutex that
// another goroutine still holds would let the next LockFile create a fresh
// mutex and bypass serialization. The set of distinct paths touched per
// session is small, so retention is cheap.
func (s *Session) Forget(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.markers, path)
}
