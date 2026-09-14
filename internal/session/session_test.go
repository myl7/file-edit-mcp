package session

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// tm is a fixed wall time for markers.
var tm = time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)

func TestLookupUnreadPath(t *testing.T) {
	s := New()
	if _, ok := s.Lookup("/data/a.txt"); ok {
		t.Fatal("Lookup on unread path reported present")
	}
	if got := s.Compare("/data/a.txt", 10, tm); got != StatusUnknown {
		t.Fatalf("Compare unread path = %v, want StatusUnknown", got)
	}
}

func TestMarkReadThenLookupCompare(t *testing.T) {
	s := New()
	s.MarkRead("/data/a.txt", 128, tm)

	m, ok := s.Lookup("/data/a.txt")
	if !ok {
		t.Fatal("Lookup after MarkRead reported missing")
	}
	if m.Size != 128 || !m.ModTime.Equal(tm) {
		t.Fatalf("Lookup marker = %+v, want {Size:128 ModTime:%v}", m, tm)
	}

	if got := s.Compare("/data/a.txt", 128, tm); got != StatusMatch {
		t.Fatalf("Compare identical = %v, want StatusMatch", got)
	}
	if got := s.Compare("/data/a.txt", 129, tm); got != StatusStale {
		t.Fatalf("Compare size drift = %v, want StatusStale", got)
	}
	if got := s.Compare("/data/a.txt", 128, tm.Add(time.Second)); got != StatusStale {
		t.Fatalf("Compare mtime drift = %v, want StatusStale", got)
	}
	// Another path stays unknown even after marking this one.
	if got := s.Compare("/data/other.txt", 128, tm); got != StatusUnknown {
		t.Fatalf("Compare untouched path = %v, want StatusUnknown", got)
	}
}

func TestMarkReadOverwrites(t *testing.T) {
	s := New()
	s.MarkRead("/a", 1, tm)
	s.MarkRead("/a", 2, tm.Add(time.Second))
	m, ok := s.Lookup("/a")
	if !ok || m.Size != 2 {
		t.Fatalf("Lookup after re-MarkRead = (%+v, %v), want latest marker {Size:2}", m, ok)
	}
}

func TestForget(t *testing.T) {
	s := New()
	s.MarkRead("/a", 1, tm)
	s.Forget("/a")
	if _, ok := s.Lookup("/a"); ok {
		t.Fatal("Lookup after Forget reported present")
	}
	if got := s.Compare("/a", 1, tm); got != StatusUnknown {
		t.Fatalf("Compare after Forget = %v, want StatusUnknown", got)
	}
	// Forgetting a never-marked path is a harmless no-op.
	s.Forget("/never-marked")
}

func TestZeroValueUsable(t *testing.T) {
	var s Session
	s.MarkRead("/a", 1, tm)
	if _, ok := s.Lookup("/a"); !ok {
		t.Fatal("zero-value Session lost marker")
	}
	var s2 Session
	unlock := s2.LockFile("/a")
	unlock()
	s2.Forget("/a")
}

func TestLockFileSerializesSamePath(t *testing.T) {
	s := New()
	const goroutines = 32
	n := 0 // guarded by the per-path lock; -race catches unsynchronized access
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			unlock := s.LockFile("/data/a.txt")
			n++
			unlock()
		}()
	}
	wg.Wait()
	if n != goroutines {
		t.Fatalf("critical section ran %d times, want %d", n, goroutines)
	}
}

func TestLockFileSamePathBlocks(t *testing.T) {
	s := New()
	unlock := s.LockFile("/a")

	acquired := make(chan struct{})
	go func() {
		u := s.LockFile("/a")
		close(acquired)
		u()
	}()

	select {
	case <-acquired:
		t.Fatal("same-path LockFile acquired while already held")
	case <-time.After(50 * time.Millisecond):
		// Still blocked, as expected.
	}

	unlock()
	select {
	case <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("same-path LockFile never acquired after unlock")
	}
}

func TestLockFileDifferentPathsIndependent(t *testing.T) {
	s := New()
	unlockA := s.LockFile("/a")

	done := make(chan struct{})
	go func() {
		unlockB := s.LockFile("/b")
		unlockB()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("LockFile(/b) blocked while only /a was held")
	}
	unlockA()
}

// TestConcurrentMixedAccess hammers every method from many goroutines over
// a small path set; run with -race it verifies the whole-state locking.
func TestConcurrentMixedAccess(t *testing.T) {
	s := New()
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			path := fmt.Sprintf("/data/f%d.txt", i%4)
			s.MarkRead(path, int64(i), tm.Add(time.Duration(i)*time.Second))
			s.Lookup(path)
			s.Compare(path, int64(i), tm)
			s.Forget(path)
			s.Lookup(path)
			unlock := s.LockFile(path)
			unlock()
		}(i)
	}
	wg.Wait()
}
