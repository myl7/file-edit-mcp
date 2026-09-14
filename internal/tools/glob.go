// glob tool handler (ARCHITECTURE §5.5): pathguard-resolved search roots, a
// WalkDir traversal (read-only, no os.Root required per §4), doublestar
// pattern matching against paths relative to the search root, mtime-desc
// ordering, and the two safety notices (200-result cap, 100k-entry scan valve).

package tools

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// globMaxResults is the §5.5 result cap: at most this many paths are
// returned; a larger match set is truncated with a "showing X of Y" note.
//
// A var (not const) purely so tests can shrink it without creating hundreds
// of files — the same pattern as lineio's limits. Production code must only
// read it. Tests that override it must not run in parallel (package tests
// run serially unless t.Parallel is used).
var globMaxResults = 200

// globScanLimit is the §5.5 traversal safety valve: once this many entries
// (matching or not, directories included) have been visited, the walk stops
// and the note declares results possibly incomplete. Mandatory on very
// large shares; also stated in the tool description. Same test-override
// contract as globMaxResults.
var globScanLimit = 100000

// scanStoppedNoteFmt is the note template for the traversal valve; %d is the
// entry count at which the scan stopped.
const scanStoppedNoteFmt = "scan stopped after %d entries; results may be incomplete"

// truncatedNoteFmt is the note template for the result cap; first %d is the
// number shown, second %d the total match count, third %d the cap itself.
const truncatedNoteFmt = "showing %d of %d matches (limit %d); narrow the pattern or set path to a subdirectory"

// globMatch pairs one matching file with its mtime for the sort below.
type globMatch struct {
	path  string
	mtime time.Time
}

// Glob implements the glob tool (§5.5). The pattern is matched with
// doublestar against the full path relative to the search root (so `**`
// crosses directories at any depth, including zero levels); directories
// themselves are never matches, only files are. Results are absolute paths
// sorted by mtime, newest first (path ascending breaks exact ties so output
// is deterministic). Traversal is read-only via filepath.WalkDir — §4
// explicitly waives the os.Root requirement for glob/grep walks, so the
// residual TOCTOU risk (a component swapped for a symlink mid-walk) is
// accepted: a listed path still has to pass the full pathguard pipeline
// before any read/write tool will touch it.
func (s *Conn) Glob(_ context.Context, _ *mcp.CallToolRequest, in GlobInput) (*mcp.CallToolResult, GlobOutput, error) {
	if in.Pattern == "" {
		return nil, GlobOutput{}, errors.New("pattern must not be empty")
	}
	if !doublestar.ValidatePattern(in.Pattern) {
		return nil, GlobOutput{}, fmt.Errorf("invalid doublestar pattern: %q", in.Pattern)
	}

	roots, err := s.searchRoots(in.Path)
	if err != nil {
		return nil, GlobOutput{}, err
	}

	var (
		matches     []globMatch
		scanned     int
		scanStopped bool
	)
	for _, root := range roots {
		if scanStopped {
			break
		}
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				// §5.5/T7: a directory we cannot read (e.g. EACCES) is
				// skipped, logged to stderr, and never fails the call.
				s.log().Warn("glob: skipping unreadable entry", "path", p, "err", err)
				if d != nil && d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			scanned++
			stop := scanned >= globScanLimit
			// Only files match (§5.5); directory entries are traversed but
			// never collected.
			if !d.IsDir() {
				if rel, relErr := filepath.Rel(root, p); relErr == nil {
					ok, mErr := doublestar.Match(in.Pattern, filepath.ToSlash(rel))
					if mErr != nil {
						return mErr
					}
					if ok {
						if info, iErr := d.Info(); iErr == nil {
							matches = append(matches, globMatch{path: p, mtime: info.ModTime()})
						} else {
							s.log().Warn("glob: cannot stat match", "path", p, "err", iErr)
						}
					}
				}
			}
			if stop {
				scanStopped = true
				return fs.SkipAll
			}
			return nil
		})
		if err != nil && !errors.Is(err, fs.SkipAll) {
			// The callback maps every walk error to skip+log, so reaching
			// here means an invalid pattern surfaced from Match (already
			// validated above) or WalkDir itself failed on the root.
			return nil, GlobOutput{}, fmt.Errorf("glob: walk %s: %w", root, err)
		}
	}

	// mtime descending; exact ties broken by path ascending for deterministic
	// output (filesystems with coarse mtime granularity make ties common).
	sort.Slice(matches, func(i, j int) bool {
		if !matches[i].mtime.Equal(matches[j].mtime) {
			return matches[i].mtime.After(matches[j].mtime)
		}
		return matches[i].path < matches[j].path
	})

	var notes []string
	if total := len(matches); total > globMaxResults {
		matches = matches[:globMaxResults]
		notes = append(notes, fmt.Sprintf(truncatedNoteFmt, globMaxResults, total, globMaxResults))
	}
	if scanStopped {
		notes = append(notes, fmt.Sprintf(scanStoppedNoteFmt, globScanLimit))
	}

	files := make([]string, len(matches))
	for i, m := range matches {
		files[i] = m.path
	}
	return nil, GlobOutput{Files: files, Note: strings.Join(notes, "; ")}, nil
}

// searchRoots resolves the optional search-start path shared by glob and
// grep: empty means all allowed directories; a given path must pass the §3
// pathguard pipeline and must be an existing directory (a file is not a
// valid search root; a missing path is ENotExist after the pipeline's
// new-file branch would otherwise let it through).
func (s *Conn) searchRoots(path string) ([]string, error) {
	if path == "" {
		return s.Guard.AllowedDirs(), nil
	}
	resolved, err := s.Guard.Validate(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return nil, mapFSErr(err, resolved)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("search path must be a directory, not a file: %s", resolved)
	}
	return []string{resolved}, nil
}
