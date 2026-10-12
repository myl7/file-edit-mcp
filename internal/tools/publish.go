// The upload commit core (ARCHITECTURE §5.9): stream a validated download
// into a same-directory temp file, byte-compare an existing target, and
// publish a missing target without any replace window — Root.Link as the
// atomic path, an exclusive-create copy as the degraded path when the
// share refuses hard links. Never a stat→rename, never an O_EXCL
// placeholder→rename: both reopen the overwrite window this design
// closes.

package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/myl7/file-edit-mcp/internal/errmsg"
	"github.com/myl7/file-edit-mcp/internal/imagedownload"
)

// downloaded is a fully-downloaded, content-validated image sitting in its
// same-directory temp file, ready for the compare/publish stage.
type downloaded struct {
	// tmpRel is the temp name relative to any Root owning the target's
	// directory (the temp lives there so Link is same-filesystem).
	tmpRel string
	// size and sha are the temp file's byte count and SHA-256 hex.
	size int64
	sha  string
	// det is the detected image type; origin is the final URL's
	// scheme://host for logs only (never the query or full URL).
	det    imagedownload.Detected
	origin string
}

// errPublishRaced marks "the target appeared between the missing-check and
// the publish attempt" (Link or the degraded create returned EEXIST): the
// attempt re-routes into the byte-compare branch.
var errPublishRaced = errors.New("target appeared concurrently")

// errWriter captures sink (storage) failures so the network-vs-storage
// split survives ConsumeBody's error path: a tee write error surfaces as a
// raw error there, indistinguishable from a body read error without this.
type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) Write(p []byte) (int, error) {
	if e.err != nil {
		return 0, e.err
	}
	n, err := e.w.Write(p)
	if err != nil {
		e.err = err
	}
	return n, err
}

// streamOnce runs one full download attempt (§5.9): create the temp file
// under a Root lease (its own §8 retry — no body is consumed yet), stream
// the response body through image validation and the SHA-256 hasher into
// it, and finalize (fsync + 0644, best effort on CIFS). The caller owns
// closing resp.Body. A network/content failure fails the item; a storage
// failure returns isStorageStreamErr(err)==true so the caller re-GETs
// from zero under the same budget — a half-written temp is never resumed.
func (s *Conn) streamOnce(ctx context.Context, resolved string, resp *imagedownload.Response) (*downloaded, error) {
	// Temp creation is retryable on its own: no body is consumed yet, and
	// every attempt re-resolves a fresh Root under its lease.
	tmpRel, err := runFSContext(s, ctx, resolved, "temp create", func() (string, error) {
		var name string
		err := s.Roots.WithRoot(resolved, func(root *os.Root, rel string) error {
			n, f, cerr := createTempIn(root, filepath.Dir(rel))
			if cerr != nil {
				return cerr
			}
			name = n
			// The exclusive create IS the claim; reopen for writing below.
			return f.Close()
		})
		return name, err
	})
	if err != nil {
		return nil, err
	}
	dl := &downloaded{tmpRel: tmpRel}

	hasher := sha256.New()
	sink := &errWriter{w: hasher}
	cleanup := func() {
		_ = s.Roots.WithRoot(resolved, func(root *os.Root, rel string) error {
			_ = root.Remove(dl.tmpRel)
			return nil
		})
	}

	// Streaming is NOT retried here: the body is single-use (§8 — a
	// consumed body never goes inside a retriable closure). Storage errors
	// propagate to the caller for the from-zero re-GET.
	streamErr := s.Roots.WithRoot(resolved, func(root *os.Root, rel string) error {
		f, ferr := root.OpenFile(dl.tmpRel, os.O_WRONLY, 0o600)
		if ferr != nil {
			return ferr
		}
		fileSink := &errWriter{w: f}
		tee := io.MultiWriter(fileSink, sink)
		n, det, cerr := s.Fetch.ConsumeBody(resp, tee)
		if cerr != nil {
			_ = f.Close()
			if fileSink.err != nil || sink.err != nil {
				return fileSinkFirst(fileSink, sink)
			}
			return cerr
		}
		if serr := f.Sync(); serr != nil {
			// fsync is best effort on CIFS (§5.2 defines atomicity by the
			// rename/link, not durability).
			s.log().Debug("fsync on upload temp failed", "path", resolved, "err", serr)
		}
		if cerr := f.Close(); cerr != nil {
			return cerr
		}
		if err := root.Chmod(dl.tmpRel, 0o644); err != nil {
			// Mode on CIFS is decorative (§5.2); best effort only.
			s.log().Debug("chmod on upload temp failed", "path", resolved, "err", err)
		}
		dl.size = n
		dl.det = det
		return nil
	})
	if streamErr != nil {
		cleanup()
		return nil, streamErr
	}
	dl.sha = hex.EncodeToString(hasher.Sum(nil))
	dl.origin = originOf(resp)
	return dl, nil
}

// fileSinkFirst returns the storage-side error (file write outranks the
// hasher, which cannot fail on its own).
func fileSinkFirst(a, b *errWriter) error {
	if a.err != nil {
		return a.err
	}
	return b.err
}

// isStorageStreamErr reports whether a streamOnce failure is
// storage-class (retry with a from-zero re-GET) rather than
// network/content-class (fail the item): imagedownload-classified errors
// are network/content; raw errors came from the tee (file/hash sink) or
// the file lifecycle, i.e. storage.
func isStorageStreamErr(err error) bool {
	var ie *imagedownload.Error
	if errors.As(err, &ie) {
		return false
	}
	var ce *errmsg.Error
	return !errors.As(err, &ce)
}

// cleanupTemp removes dl's temp name, best effort (§5.9: request failure
// cleans its own temps; committed final names are never touched).
func (s *Conn) cleanupTemp(resolved, tmpRel string) {
	_ = s.Roots.WithRoot(resolved, func(root *os.Root, rel string) error {
		_ = root.Remove(tmpRel)
		return nil
	})
}

// publishOutcome is the runFSContext payload of one publish attempt.
type publishOutcome struct {
	status  string
	warning string
}

// publishItem decides skipped/created for one fully-downloaded image under
// the caller's §7 locks (§5.9): an existing target is byte-compared (same
// → skipped, different → EExistsConflict, unstable during the compare →
// ETargetChanged); a missing target goes through publishNoReplace, and a
// publish that loses a create race re-routes into the compare inside the
// same attempt. The whole decision runs inside the §8 context retry so a
// mid-decision backend error re-runs the attempt against the CURRENT
// state — the reconciliation the post-error ambiguity needs.
func (s *Conn) publishItem(ctx context.Context, resolved string, dl *downloaded) (status string, warning string, err error) {
	compare := func(root *os.Root, rel string, info os.FileInfo) (publishOutcome, error) {
		if info.IsDir() {
			return publishOutcome{}, errmsg.EIsDir(resolved)
		}
		if !info.Mode().IsRegular() {
			// A symlink/special at the resolved name appeared after
			// pathguard's realpath pass: never write through or over it.
			return publishOutcome{}, errmsg.EExistsConflict(resolved)
		}
		same, cerr := s.compareExisting(ctx, root, rel, resolved, info, dl)
		if cerr != nil {
			return publishOutcome{}, cerr
		}
		if same {
			return publishOutcome{status: "skipped"}, nil
		}
		return publishOutcome{}, errmsg.EExistsConflict(resolved)
	}

	var outcome publishOutcome
	_, err = runFSContext(s, ctx, resolved, "publish", func() (publishOutcome, error) {
		outcome = publishOutcome{} // each §8 attempt starts from a blank slate
		err := s.Roots.WithRoot(resolved, func(root *os.Root, rel string) error {
			info, err := root.Lstat(rel)
			switch {
			case err == nil:
				out, cerr := compare(root, rel, info)
				if cerr != nil {
					return cerr
				}
				outcome = out
				return nil
			case errors.Is(err, fs.ErrNotExist):
				st, warn, perr := s.publishNoReplace(root, rel, resolved, dl)
				if errors.Is(perr, errPublishRaced) {
					// The target appeared between the missing-check and
					// the publish: this attempt becomes the compare.
					info2, err2 := root.Lstat(rel)
					if err2 != nil {
						return err2
					}
					out2, cerr := compare(root, rel, info2)
					if cerr != nil {
						return cerr
					}
					outcome = out2
					return nil
				}
				if perr != nil {
					return perr
				}
				outcome = publishOutcome{status: st, warning: warn}
				return nil
			default:
				return err
			}
		})
		return outcome, err
	})
	if err != nil {
		return "", "", err
	}
	return outcome.status, outcome.warning, nil
}

// compareExisting streams the existing target (bounded by the same
// per-image cap) and decides skipped vs conflict, with the read fenced by
// before/after stats: size or mtime moving during the compare is
// ETargetChanged (§5.9 — retry the upload; there is no read to redo).
func (s *Conn) compareExisting(ctx context.Context, root *os.Root, rel, resolved string, before os.FileInfo, dl *downloaded) (bool, error) {
	f, err := root.Open(rel)
	if err != nil {
		return false, err
	}
	defer f.Close()
	hasher := sha256.New()
	n, err := io.Copy(hasher, io.LimitReader(f, s.UploadLimits.MaxImageBytes+1))
	if err != nil {
		return false, err
	}
	if n > s.UploadLimits.MaxImageBytes {
		// An over-cap existing target can never equal a compliant
		// download: a conflict, not a size problem.
		return false, errmsg.EExistsConflict(resolved)
	}
	after, err := root.Lstat(rel)
	if err != nil {
		return false, err
	}
	if after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		return false, errmsg.ETargetChanged(resolved)
	}
	if n != before.Size() {
		// The read itself saw a different length than the pre-stat on a
		// file whose post-stat matches the pre-stat: moved mid-read.
		return false, errmsg.ETargetChanged(resolved)
	}
	if n != dl.size || hex.EncodeToString(hasher.Sum(nil)) != dl.sha {
		return false, nil // stable but different: a plain conflict
	}
	return true, nil
}

// publishNoReplace lands dl's temp file at rel with no replace window:
// Root.Link (atomic: the final name appears fully-formed or not at all),
// then the temp name is removed. A Link refusal in the
// "share does not support hard links" family degrades to an exclusive
// create + copy — no-replace survives via O_EXCL, whole-file atomicity
// does not, and the item carries the EAtomicDegraded warning so the mode
// is visible (§5.9 degraded mode).
func (s *Conn) publishNoReplace(root *os.Root, rel, resolved string, dl *downloaded) (status, warning string, err error) {
	// The temp source must still be our regular file (§5.9: Lstat, never
	// a blind link through a swapped name).
	if info, err := root.Lstat(dl.tmpRel); err != nil {
		return "", "", errmsg.EBackend(fmt.Errorf("upload temp file vanished before publish"))
	} else if !info.Mode().IsRegular() || info.Size() != dl.size {
		return "", "", errmsg.EBackend(fmt.Errorf("upload temp file changed before publish"))
	}

	err = s.link(root, dl.tmpRel, rel)
	switch {
	case err == nil:
		// Link success IS the commit; a failed temp-name removal is a
		// cleanup notice, never a rollback.
		if rerr := root.Remove(dl.tmpRel); rerr != nil {
			s.log().Warn("upload temp cleanup failed", "path", resolved, "err", rerr)
			return "created", errmsg.CodeTempCleanup, nil
		}
		return "created", "", nil
	case errors.Is(err, fs.ErrExist):
		return "", "", errPublishRaced
	case isUnsupportedLink(err):
		s.log().Warn("hard-link publish unsupported; degraded no-replace mode", "path", resolved, "err", err)
		st, derr := s.degradedPublish(root, rel, dl.tmpRel, dl.size)
		if derr != nil {
			if errors.Is(derr, fs.ErrExist) {
				return "", "", errPublishRaced
			}
			return "", "", derr
		}
		// The temp file is now redundant either way.
		if rerr := root.Remove(dl.tmpRel); rerr != nil {
			s.log().Warn("upload temp cleanup failed", "path", resolved, "err", rerr)
			return st, errmsg.CodeTempCleanup + ";" + errmsg.CodeAtomicDegraded, nil
		}
		return st, errmsg.CodeAtomicDegraded, nil
	default:
		// Backend-class errors (EIO/ETIMEDOUT/...) may have taken effect —
		// the caller's retry reconciles against the current state; other
		// errors fail the item as-is.
		return "", "", err
	}
}

// degradedPublish copies the completed temp into an exclusively created
// final name (§5.9 degraded mode). O_EXCL keeps the never-overwrite
// contract; the loss is atomicity — a crash mid-copy can leave a partial
// final file, which is why this mode is warned about, logged, and only
// entered when Link is refused. A copy failure removes the partial final
// best effort; if even that fails the error is returned loudly.
func (s *Conn) degradedPublish(root *os.Root, rel, tmpRel string, size int64) (string, error) {
	dst, err := root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return "", err
	}
	committed := false
	defer func() {
		if !committed {
			_ = root.Remove(rel)
		}
	}()
	src, err := root.Open(tmpRel)
	if err != nil {
		return "", err
	}
	defer src.Close()
	buf := make([]byte, s.UploadLimits.copyBufferSize())
	n, err := io.CopyBuffer(dst, src, buf)
	if err != nil {
		return "", err
	}
	if serr := dst.Sync(); serr != nil {
		s.log().Debug("fsync on degraded upload target failed", "err", serr)
	}
	if cerr := dst.Close(); cerr != nil {
		return "", cerr
	}
	if n != size {
		return "", fmt.Errorf("degraded copy wrote %d of %d bytes", n, size)
	}
	if err := root.Chmod(rel, 0o644); err != nil {
		s.log().Debug("chmod on degraded upload target failed", "err", err)
	}
	committed = true
	return "created", nil
}

// isUnsupportedLink classifies Link failures that leave the degraded path
// open (§5.9): the no-link errno family (ENOSYS/EOPNOTSUPP/ENOTSUP), the
// permission shapes whose create may still succeed (EPERM/EACCES), and
// the link-count/cross-device oddities (EMLINK/EXDEV) that cannot mean
// "target exists".
func isUnsupportedLink(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	switch errno {
	// ENOTSUP is deliberately absent: it is a same-value alias of
	// EOPNOTSUPP on both Linux and macOS, and a duplicate case is a
	// compile error on those platforms.
	case syscall.ENOSYS, syscall.EOPNOTSUPP,
		syscall.EPERM, syscall.EACCES, syscall.EMLINK, syscall.EXDEV:
		return true
	}
	return false
}

// originOf renders a response's final URL as scheme://host for server
// logs (§5.9: the query — signatures — never reaches logs or results).
func originOf(resp *imagedownload.Response) string {
	if resp == nil || resp.URL == nil {
		return ""
	}
	return resp.URL.Scheme + "://" + resp.URL.Host
}

// copyBufferSize returns the per-worker copy buffer size (§5.9: 32 KiB).
func (l *UploadLimits) copyBufferSize() int {
	if l.MaxImageBytes > 0 && l.MaxImageBytes < int64(32<<10) {
		return int(l.MaxImageBytes)
	}
	return 32 << 10
}
