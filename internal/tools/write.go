// write tool handler (ARCHITECTURE §5.2): the freshness gate shared with
// edit (unified: never-read and stale both report EStaleRead), the O_EXCL
// claim for new files, and the atomic temp-file + rename write path shared
// with edit/multi_edit.

package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/myl7/file-edit-mcp/internal/errmsg"
	"github.com/myl7/file-edit-mcp/internal/session"
)

// tempPrefix is the §5.2 same-directory temp-file prefix. The temp file
// lives in the target's directory so the rename onto the target is atomic
// (same filesystem, one inode swap).
const tempPrefix = ".file-edit-mcp-"

// tempAttempts bounds random-name generation: collisions are theoretically
// possible (O_EXCL loop), practically never hit twice.
const tempAttempts = 8

// writeAck is the (informal) structured output of write: no shape is
// specified in §5, so Out stays `any` and this is pure information.
type writeAck struct {
	BytesWritten int64 `json:"bytes_written"`
}

// Write implements the write tool (§5.2): create-or-rewrite, never append.
// Existing targets pass the SAME freshness gate as edit (unified in this
// change): the session marker must exist AND still match the file's
// size/mtime — never read is the never-read EStaleRead variant, read but
// changed the stale variant — followed by the same pre-write re-stat
// editFlow runs (both checks mirror edit.go). New targets are claimed with
// O_EXCL (a lost race reports the never-read variant too: the file now
// exists with no read behind it), and the content lands via the atomic
// temp+rename path. The whole operation runs under the per-path session
// lock (§7).
func (s *Conn) Write(_ context.Context, _ *mcp.CallToolRequest, in WriteInput) (*mcp.CallToolResult, any, error) {
	resolved, err := s.Guard.Validate(in.FilePath)
	if err != nil {
		return nil, nil, err
	}

	unlock := s.Sess.LockFile(resolved)
	defer unlock()

	// Step 1: does the target exist, and with which mode/size/mtime? The
	// stat doubles as the gate's "current state" sample (editFlow step 1).
	var (
		exists bool
		mode   = os.FileMode(0o644) // §5.2: new files get 0644
		size   int64
		mtime  time.Time
	)
	err = s.Retry.Do(func() error {
		root, rel, err := s.Roots.Resolve(resolved)
		if err != nil {
			return err
		}
		info, err := root.Stat(rel)
		if err == nil {
			if info.IsDir() {
				return errmsg.EIsDir(resolved)
			}
			exists, mode, size, mtime = true, info.Mode().Perm(), info.Size(), info.ModTime()
			return nil
		}
		if errors.Is(err, fs.ErrNotExist) {
			// A missing parent was already rejected by pathguard (§3 step 8
			// → EParentMissing); reaching here means only the file is new.
			exists = false
			return nil
		}
		return err
	})
	if err != nil {
		return nil, nil, mapFSErr(err, resolved)
	}

	claimed := false
	if exists {
		// §5.2 (unified gate): an existing target demands a CURRENT read —
		// the same fence editFlow applies at its step 2. No marker at all is
		// the never-read EStaleRead variant (with no read knowledge, any
		// current state counts as changed-since-last-known); a marker that no
		// longer matches the stat above is the stale variant. The remedy is
		// identical either way: read, then write.
		switch s.Sess.Compare(resolved, size, mtime) {
		case session.StatusUnknown:
			return nil, nil, errmsg.EStaleReadNeverRead(resolved)
		case session.StatusStale:
			return nil, nil, errmsg.EStaleRead(resolved)
		}
		// Pre-write re-check (editFlow step 4 mirrored): a writer that moved
		// the file between the stat above and the rename — cross-token or
		// out-of-band, invisible to this token's per-path lock — makes this
		// EStaleRead instead of a silent wholesale overwrite of its work.
		cursize, curmtime, _, err := s.statFile(resolved)
		if err != nil {
			return nil, nil, mapFSErr(err, resolved)
		}
		if cursize != size || !curmtime.Equal(mtime) {
			return nil, nil, errmsg.EStaleRead(resolved)
		}
	} else {
		// §5.2: claim the name exclusively; a concurrent creator beats us to
		// the O_EXCL and the call reports the never-read EStaleRead variant
		// (the file now exists with no read behind it).
		err = s.Retry.Do(func() error {
			root, rel, err := s.Roots.Resolve(resolved)
			if err != nil {
				return err
			}
			f, err := root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
			if err != nil {
				if errors.Is(err, fs.ErrExist) {
					return errmsg.EStaleReadNeverRead(resolved)
				}
				if errors.Is(err, fs.ErrNotExist) {
					// The parent vanished between validation and now.
					return errmsg.EParentMissing(filepath.Dir(resolved))
				}
				return err
			}
			return f.Close()
		})
		if err != nil {
			return nil, nil, mapFSErr(err, resolved)
		}
		claimed = true
	}

	if err := s.atomicWrite(resolved, []byte(in.Content), mode); err != nil {
		if claimed {
			// The claim is an empty file we created; on failure restore the
			// pre-call state so a retry is not blocked by the never-read
			// EStaleRead variant against a file the caller never saw. Best
			// effort.
			_ = s.Retry.Do(func() error {
				root, rel, err := s.Roots.Resolve(resolved)
				if err != nil {
					return err
				}
				return root.Remove(rel)
			})
		}
		return nil, nil, mapFSErr(err, resolved)
	}

	// §5.2: a successful write means the content is known — record the
	// marker (edits may follow without a fresh read).
	s.markKnown(resolved, int64(len(in.Content)))
	return nil, any(writeAck{BytesWritten: int64(len(in.Content))}), nil
}

// markKnown records a fresh post-write marker for resolved. If the stat
// fails (e.g. a late backend hiccup) the write still succeeded, so a
// deliberately stale marker is recorded (size only, zero mtime): the next
// modifying call then reports EStaleRead and forces a re-read — the safe
// direction.
func (s *Conn) markKnown(resolved string, fallbackSize int64) (int64, time.Time) {
	var (
		size  int64
		mtime time.Time
	)
	st, err := runFS(s, resolved, func() (os.FileInfo, error) {
		root, rel, err := s.Roots.Resolve(resolved)
		if err != nil {
			return nil, err
		}
		return root.Stat(rel)
	})
	if err != nil {
		s.log().Warn("post-write stat failed; recording stale marker", "path", resolved, "err", err)
		size, mtime = fallbackSize, time.Time{}
	} else {
		size, mtime = st.Size(), st.ModTime()
	}
	s.Sess.MarkRead(resolved, size, mtime)
	return size, mtime
}

// statFile stats resolved through the Root/retry pipeline.
func (s *Conn) statFile(resolved string) (size int64, mtime time.Time, mode os.FileMode, err error) {
	st, err := runFS(s, resolved, func() (os.FileInfo, error) {
		root, rel, err := s.Roots.Resolve(resolved)
		if err != nil {
			return nil, err
		}
		return root.Stat(rel)
	})
	if err != nil {
		return 0, time.Time{}, 0, err
	}
	return st.Size(), st.ModTime(), st.Mode().Perm(), nil
}

// atomicWrite implements the §5.2 write path: a same-directory temp file
// (prefix .file-edit-mcp-), full content written, chmod to the final mode
// (the caller decides: original mode for existing targets, 0644 for new),
// then rename over the target. Any failure removes the temp file; the
// target either keeps its old content or (for new files) was claimed
// separately and is cleaned by the caller.
//
// os.Root has no CreateTemp in Go 1.27, so the temp file is created with
// OpenFile(O_WRONLY|O_CREATE|O_EXCL) plus a random name — the O_EXCL loop
// stays inside the Root's openat confinement and leaves no TOCTOU window;
// no os.CreateTemp/os.Rename fallback is needed.
func (s *Conn) atomicWrite(resolved string, data []byte, mode os.FileMode) error {
	return s.Retry.Do(func() error {
		root, rel, err := s.Roots.Resolve(resolved)
		if err != nil {
			return err
		}

		tmpRel, f, err := createTempIn(root, filepath.Dir(rel))
		if err != nil {
			return err
		}
		keep := false
		defer func() {
			if !keep {
				// §5.2: failure cleans the temp file; the target is
				// untouched because the rename never ran. Best effort.
				_ = root.Remove(tmpRel)
			}
		}()

		if _, err := f.Write(data); err != nil {
			_ = f.Close()
			return err
		}
		// fsync is best effort: on CIFS it is unreliable, and §5.2 defines
		// atomicity by the rename, not by durability.
		_ = f.Sync()
		if err := f.Close(); err != nil {
			return err
		}
		if err := root.Chmod(tmpRel, mode); err != nil {
			// §5.2: mode on CIFS is decorative — best effort, never fatal.
			s.log().Debug("chmod on temp file failed", "path", resolved, "err", err)
		}
		if err := s.rename(root, tmpRel, rel); err != nil {
			return err
		}
		keep = true
		return nil
	})
}

// createTempIn creates an exclusive temp file inside dirRel (Root-relative,
// "." for the Root itself) and returns its Root-relative name plus the open
// handle. The O_EXCL loop retries random name collisions.
func createTempIn(root *os.Root, dirRel string) (string, *os.File, error) {
	prefix := tempPrefix
	if dirRel != "." && dirRel != string(filepath.Separator) {
		prefix = dirRel + "/" + tempPrefix
	}
	var lastErr error
	for i := 0; i < tempAttempts; i++ {
		name := prefix + randomSuffix()
		f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			return name, f, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", nil, err
		}
		lastErr = err
	}
	return "", nil, fmt.Errorf("no free temp name after %d attempts: %w", tempAttempts, lastErr)
}

// randomSuffix returns 8 random bytes hex-encoded (16 chars). crypto/rand
// never blocks after init on Linux/macOS; the fallback only covers
// pathological early-boot failures so the temp loop can still proceed.
func randomSuffix() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
