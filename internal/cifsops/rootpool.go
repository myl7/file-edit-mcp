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
// OpenRoot, Open, OpenFile, Stat, ReadFile, Rename, Remove, Chmod, Close.
// os.Root has NO CreateTemp, so §5.2 temp files are created with
// OpenFile(O_WRONLY|O_CREATE|O_EXCL) plus a random name (see tools layer) —
// this stays inside the Root's confinement and needs no os.CreateTemp
// fallback, so no TOCTOU window is reintroduced. Rename exists on Root, so
// os.Rename is never used either. No cgo anywhere.
type RootPool struct {
	// mu guards roots; it is held while opening/closing Roots but never
	// across a caller's use of a Root (os.Root is itself goroutine-safe).
	mu sync.Mutex
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

// Resolve is Open plus the Root-relative name for path: the value to pass
// to the returned Root's methods. The relative name never contains ".."
// because owner() only matches real subpaths. For path equal to an allowed
// directory the name is "." (the directory itself).
func (p *RootPool) Resolve(path string) (*os.Root, string, error) {
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
// Residual race, accepted per §4: ReopenAll may close a Root another
// goroutine is still using (concurrent handlers on different paths share
// one Root). That operation fails with a closed-Root error (unknown class,
// not retried) while the in-flight caller's own path is unaffected by the
// mount jitter; the next tool call resolves a fresh Root via Open. Same-path
// callers never see this because handlers hold the session lock across the
// whole operation.
func (p *RootPool) ReopenAll() error {
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
// strip cannot fail and never yields "..".
func relName(dir, path string) string {
	if path == dir {
		return "."
	}
	rel := strings.TrimPrefix(path, dir+"/")
	if cleaned := filepath.Clean(rel); cleaned != rel {
		// Cannot happen for validated paths; keep the Root contract safe.
		return cleaned
	}
	return rel
}
