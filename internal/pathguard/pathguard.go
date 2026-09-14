// Package pathguard implements the ARCHITECTURE §3 path validation
// pipeline: NUL-byte rejection, best-effort tilde expansion, Windows
// drive-letter rejection, absolute-only, Clean+Abs, realpath resolution
// gated by the allowed-root prefix comparison (the sole authority), and
// the ENOENT parent-directory branch used by new-file creation.
//
// A Guard is immutable after New and safe for concurrent use.
package pathguard

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/myl7/file-edit-mcp/internal/errmsg"
)

// windowsDriveRe matches the §3 step-3 shape: a drive letter followed by
// a slash or backslash (e.g. C:\x or c:/x).
var windowsDriveRe = regexp.MustCompile(`^[A-Za-z]:[\\/]`)

// Guard holds the startup-canonicalized allowed directories and validates
// request paths against them. Create with New; the zero value has no
// allowed roots and rejects everything with EOutside.
type Guard struct {
	// dirs are the allowed directories after Abs+Clean+EvalSymlinks,
	// deduplicated. Never mutated after New (ARCHITECTURE §3).
	dirs []string
}

// New canonicalizes every entry of allowedDirs (Abs, which also Cleans,
// then EvalSymlinks) and caches the result. An empty list, a missing
// directory, or a path that is not a directory is a startup error.
//
// These errors are operator-facing (written to stderr at boot), not
// model-visible, so they are plain wrapped errors; the model-visible
// catalog lives in errmsg.
func New(allowedDirs []string) (*Guard, error) {
	if len(allowedDirs) == 0 {
		return nil, errors.New("pathguard: at least one allowed directory is required")
	}
	seen := make(map[string]bool, len(allowedDirs))
	dirs := make([]string, 0, len(allowedDirs))
	for _, dir := range allowedDirs {
		abs, err := filepath.Abs(dir) // resolves relative to CWD, also Cleans
		if err != nil {
			return nil, fmt.Errorf("pathguard: allowed dir %q: %w", dir, err)
		}
		real, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return nil, fmt.Errorf("pathguard: allowed dir %q: %w", dir, err)
		}
		info, err := os.Stat(real)
		if err != nil {
			return nil, fmt.Errorf("pathguard: allowed dir %q: %w", dir, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("pathguard: allowed dir %q is not a directory", dir)
		}
		if !seen[real] {
			seen[real] = true
			dirs = append(dirs, real)
		}
	}
	return &Guard{dirs: dirs}, nil
}

// AllowedDirs returns a copy of the canonicalized allowed directories.
func (g *Guard) AllowedDirs() []string {
	out := make([]string, len(g.dirs))
	copy(out, g.dirs)
	return out
}

// Validate runs the §3 pipeline on rawPath and returns the path all later
// operations must use: the fully resolved realpath (or, for not-yet-existing
// files, parent-realpath + base). Steps run strictly in order; the first
// failure returns an errmsg catalog error.
func (g *Guard) Validate(rawPath string) (string, error) {
	// Step 1: a NUL byte can smuggle a second path through C APIs.
	if strings.IndexByte(rawPath, 0) >= 0 {
		return "", errmsg.ENulByte()
	}

	// Step 2: expand ~ and ~user to absolute home paths; on failure keep
	// the path as-is (step 4 will reject it as relative).
	p := expandTilde(rawPath)

	// Step 3: reject Windows drive-letter shapes outright.
	if windowsDriveRe.MatchString(p) {
		return "", errmsg.EWindowsPath(p, g.dirs)
	}

	// Step 4: absolute paths only; relative paths are never interpreted
	// against the allowed roots.
	if !filepath.IsAbs(p) {
		return "", errmsg.ERelative(g.dirs)
	}

	// Step 5: Clean + Abs collapse ., .., and duplicate separators. This
	// must happen BEFORE the step-7 comparison so that e.g.
	// root/sub/../x cannot smuggle a different prefix through.
	p = filepath.Clean(p)
	if abs, err := filepath.Abs(p); err == nil {
		p = abs // no-op Clean for an already-absolute path; kept per spec
	}

	// Step 6 is deliberately NOT a gate (§3 as revised): comparing the
	// cleaned form against the allowed roots is a fast path with no veto
	// power. A cleaned form matching no allowed root must fall through to
	// step 7, because the input may enter an allowed directory through a
	// symlink alias (macOS /tmp → /private/tmp, autofs /net/*, ...). No
	// fast-path check is computed here since the step-7 comparison below
	// runs for every path anyway and is the only authority — removing it
	// changes nothing about which paths are allowed (fail-closed).

	// Step 7: realpath resolves every existing segment (symlinks). The
	// prefix comparison of the RESOLVED path is final: outside → EOutside
	// (even when the cleaned form looked in-bounds, e.g. an in-root symlink
	// pointing out), inside → allowed (even when the cleaned form matched
	// no root, e.g. alias spellings of an allowed directory). All later
	// operations use the resolved result.
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Step 8: ENOENT means the final component does not exist;
			// validate exactly one parent level (new-file branch).
			return g.validateNewPath(p)
		}
		// Other EvalSymlinks failures (ENOTDIR, ELOOP, EACCES, ...) mean
		// the path cannot resolve to an existing file. §8 files ENOTDIR
		// under definite-class errors reported per §6, so they map to
		// ENotExist rather than a retryable backend error.
		return "", errmsg.ENotExist(p)
	}
	// Second prefix gate on the resolved path: a symlink may lead outside.
	if !g.anyWithin(real) {
		return "", errmsg.EOutside(real, g.dirs)
	}
	return real, nil
}

// validateNewPath is the §3 step-8 ENOENT branch: the final component is
// missing, so the parent directory (one level only) must exist and its
// REALPATH must lie inside the allowed roots — the parent's resolved form
// is authoritative (alias spellings of an allowed directory pass; parents
// that resolve outside are rejected). New-file creation passes here. The
// returned path is parent-realpath + base so symlinked parents (including
// aliases of the allowed root itself) are resolved.
func (g *Guard) validateNewPath(p string) (string, error) {
	parent := filepath.Dir(p)
	parentReal, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", errmsg.EParentMissing(parent)
	}
	if !g.anyWithin(parentReal) {
		return "", errmsg.EOutside(parentReal, g.dirs)
	}
	return filepath.Join(parentReal, filepath.Base(p)), nil
}

// anyWithin reports whether path equals or lies under any allowed dir.
func (g *Guard) anyWithin(path string) bool {
	for _, dir := range g.dirs {
		if within(dir, path) {
			return true
		}
	}
	return false
}

// within is the §3 prefix comparison: equal, or a subpath via the
// dir+"/" prefix. Applied to realpath results it is the sole authority of
// the pipeline (§3 step 7 final gate; step 8 uses it on the parent's
// realpath). The root directory "/" is special-cased because "/" + "/"
// is "//", which never prefixes a real POSIX path (the double-slash
// boundary called out in §3).
//
// Both arguments are expected to be Cleaned absolute paths; byte
// comparison is intentional (no case folding even on case-insensitive
// filesystems), matching the spec.
func within(dir, path string) bool {
	if dir == "/" {
		return path == "/" || strings.HasPrefix(path, "/")
	}
	return path == dir || strings.HasPrefix(path, dir+"/")
}

// expandTilde expands a leading "~" or "~user" to an absolute home path
// (§3 step 2). Any expansion failure falls through: the path continues
// unchanged as an ordinary (relative) path and is rejected by step 4.
// os/user works without cgo (parses /etc/passwd), keeping the build CGO-free.
func expandTilde(p string) string {
	if p == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
		return p
	}
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
		return p
	}
	if len(p) > 1 && p[0] == '~' {
		rest := p[1:]
		name, tail := rest, ""
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			name, tail = rest[:i], rest[i:] // tail keeps the leading '/'
		}
		if name != "" {
			if u, err := user.Lookup(name); err == nil && u.HomeDir != "" {
				return filepath.Join(u.HomeDir, tail)
			}
		}
	}
	return p
}
