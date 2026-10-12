// RootPool: one lazily-opened *os.Root per allowed directory (ARCHITECTURE §4).
//
// Every file open/create/stat/rename performed by the tools layer goes
// through a Root so the kernel enforces openat confinement on Linux (§4),
// closing the "symlink swapped in after validation" TOCTOU window. Roots are
// opened on first use and cached; ReopenAll closes and reopens every cached
// Root so the Retrier's OnRetry hook can re-trigger the automount after a
// backend-class error (§8).

package cifsops

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// RootPool owns the per-allowed-directory os.Root handles. Create with
// NewRootPool; a RootPool is safe for concurrent use.
//
// Go 1.27 os.Root method set used here (verified with `go doc os.Root`):
// OpenRoot, Open, OpenFile, Stat, ReadFile, Rename, Remove, Chmod, Link,
// Close. os.Root has NO CreateTemp, so §5.2 temp files are created with
// OpenFile(O_WRONLY|O_CREATE|O_EXCL) plus a random name (see tools layer) —
// this stays inside the Root's confinement and needs no os.CreateTemp
// fallback, so no TOCTOU window is reintroduced. Rename and Link exist on
// Root, so os.Rename/os.Link are never used either. No cgo anywhere.
//
// Two access forms exist (§4):
//
//   - WithRoot (preferred): op runs under a READ lease; ReopenAll takes the
//     matching WRITE lease, so a reopen waits for in-flight operations
//     instead of closing a Root under them. The upload pipeline requires
//     this — a streaming download holds its lease for seconds, and the old
//     close-under-use race would fail it spuriously.
//   - Resolve (legacy): returns the cached Root for callers that hold an
//     outer serialization (the pre-lease tools did). ReopenAll may close a
//     Resolve-handed Root mid-use; the residual race is the documented §4
//     behavior. New code uses WithRoot.
type RootPool struct {
	// mu guards roots; it is held while opening/closing Roots but never
	// across a caller's use of a Root (os.Root is itself goroutine-safe).
	mu sync.Mutex
	// lease is the WithRoot read/write lease: operations hold the read
	// side while using a Root, ReopenAll holds the write side while
	// swapping them. It is never held across Resolve itself (Resolve users
	// accept the legacy race).
	lease sync.RWMutex
	// dirs are the canonicalized allowed directories (startup-fixed; the
	// same list pathguard validated against). Used for prefix matching.
	dirs []string
	// roots caches the opened Root per allowed directory, lazily populated.
	// A dir absent from the map simply has not been opened yet.
	roots map[string]*os.Root
}

// NewRootPool returns a pool over the canonicalized allowed directories.
// Nothing is opened eagerly (§4: lazy); the first Open for a directory
// opens its Root. The caller passes the pathguard-canonicalized list
// (Abs+Clean+EvalSymlinks) — NewRootPool does not re-resolve.
func NewRootPool(dirs []string) *RootPool {
	cp := make([]string, len(dirs))
	copy(cp, dirs)
	return &RootPool{dirs: cp, roots: make(map[string]*os.Root, len(cp))}
}

// Open returns the *os.Root owning path: the Root of the allowed directory
// path lies within (§4). path must already have passed the pathguard
// pipeline; prefix matching here is a routing decision, not a security
// check. Opening is lazy: the first call for a directory opens and caches
// its Root; later calls return the cached handle.
func (p *RootPool) Open(path string) (*os.Root, error) {
	root, _, err := p.Resolve(path)
	return root, err
}

// WithRoot resolves path, runs op with the resolved Root and Root-relative
// name under a READ lease, and returns op's error (§4). While op runs,
// ReopenAll cannot swap the Roots: it blocks on the matching write lease
// until every in-flight WithRoot operation finishes. That is the point —
// the §4 close-under-use race of the legacy Resolve form cannot hit a
// WithRoot caller.
//
// Rules for op (they follow from the lease):
//   - op must be one bounded filesystem step (or a short sequence), not an
//     unbounded wait: a ReopenAll triggered by another goroutine's backend
//     retry will queue behind the lease until op returns.
//   - op must NOT run a §8 Retrier that fires OnRetry (ReopenAll) — the
//     reopen would deadlock on the lease this op still holds. Structure
//     retrying operations as WithRoot inside each retried attempt, never
//     around the retrier.
func (p *RootPool) WithRoot(path string, op func(root *os.Root, rel string) error) error {
	p.lease.RLock()
	defer p.lease.RUnlock()
	root, rel, err := p.resolve(path)
	if err != nil {
		return err
	}
	return op(root, rel)
}

// Resolve is Open plus the Root-relative name for path: the value to pass
// to the returned Root's methods. The relative name never contains ".."
// because owner() only matches real subpaths. For path equal to an allowed
// directory the name is "." (the directory itself).
//
// Legacy form (§4): the Root is handed out without a lease, so a
// concurrent ReopenAll may close it mid-use. Prefer WithRoot.
func (p *RootPool) Resolve(path string) (*os.Root, string, error) {
	return p.resolve(path)
}

// resolve is the Resolve body under p.mu (lazy open + cache).
func (p *RootPool) resolve(path string) (*os.Root, string, error) {
	dir := p.owner(path)
	if dir == "" {
		return nil, "", fmt.Errorf("cifsops: path %s is outside every pooled root", path)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if root, ok := p.roots[dir]; ok {
		return root, relName(dir, path), nil
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, "", err
	}
	p.roots[dir] = root
	return root, relName(dir, path), nil
}

// ReopenAll closes every currently-open Root and opens fresh ones,
// returning the joined reopen errors (a broken reopen leaves that
// directory uncached; the next Open retries the open).
//
// This is the §8 automount re-trigger: os.OpenRoot on the directory path
// blocks in the kernel until the automount is live again. Directories that
// were never opened stay lazy — their next Open is a fresh open anyway.
//
// It runs under the WithRoot WRITE lease: in-flight WithRoot operations
// finish first, so no operation observes a Root closed underneath it (the
// Resolve legacy form still carries the residual race documented on it).
// The lease also serializes concurrent reopens; the write lease is held
// for the whole close+reopen pass so a mid-pass Open never caches a Root
// the pass is about to replace.
func (p *RootPool) ReopenAll() error {
	p.lease.Lock()
	defer p.lease.Unlock()

	p.mu.Lock()
	opened := make(map[string]*os.Root, len(p.roots))
	for dir, root := range p.roots {
		opened[dir] = root
	}
	p.roots = make(map[string]*os.Root, len(opened))
	p.mu.Unlock()

	// Close and reopen outside the map lock so Open() never blocks on a
	// close, and never observes a closed handle in the cache.
	var reopenErr error
	for dir := range opened {
		// Close errors (e.g. double close) are irrelevant to recovery.
		_ = opened[dir].Close()
		root, err := os.OpenRoot(dir)
		if err != nil {
			reopenErr = errors.Join(reopenErr, fmt.Errorf("cifsops: reopening root %s: %w", dir, err))
			continue
		}
		p.mu.Lock()
		p.roots[dir] = root
		p.mu.Unlock()
	}
	return reopenErr
}

// owner returns the allowed directory path lies within: the longest matching
// prefix (so nested allowed directories pick the tightest root), or "" when
// path is outside every pooled root. The comparison mirrors pathguard §3
// step 6: equal or dir+"/"-prefixed, with "/" matching every absolute path.
func (p *RootPool) owner(path string) string {
	best := ""
	for _, dir := range p.dirs {
		if !withinPrefix(dir, path) {
			continue
		}
		if len(dir) > len(best) {
			best = dir
		}
	}
	return best
}

// withinPrefix is the §3 step-6 prefix rule (see pathguard.within).
func withinPrefix(dir, path string) bool {
	if dir == "/" {
		return path == "/" || strings.HasPrefix(path, "/")
	}
	return path == dir || strings.HasPrefix(path, dir+"/")
}

// relName converts an absolute path under dir into its Root-relative name.
// owner() guarantees path is dir itself or a true subpath, so the prefix
// strip cannot fail and never yields "..". The "/" allowed root needs its
// own strip: dir+"/" would be "//", which never prefixes a real POSIX
// path (the same double-slash boundary pathguard §3 special-cases).
func relName(dir, path string) string {
	if path == dir {
		return "."
	}
	var rel string
	if dir == "/" {
		rel = strings.TrimPrefix(path, "/")
	} else {
		rel = strings.TrimPrefix(path, dir+"/")
	}
	if cleaned := filepath.Clean(rel); cleaned != rel {
		// Cannot happen for validated paths; keep the Root contract safe.
		return cleaned
	}
	return rel
}
