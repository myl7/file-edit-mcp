// mkdir tool handler (ARCHITECTURE §5.8): single-level, idempotent
// directory creation under the Guard, with the §7 lock order and the §8
// retry policy driven by a cancellable budget.

package tools

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/myl7/file-edit-mcp/internal/errmsg"
)

// Mkdir implements the mkdir tool (§5.8): create exactly one directory
// level. The pathguard pipeline is the single-level gate — an existing
// target resolves through realpath, a missing target takes the new-path
// branch whose parent must already exist — so no separate recursion
// policy lives here. Idempotency: an existing directory reports "exists";
// only a concurrent creator racing us to Mkdir can surface EEXIST, and
// the re-stat then decides exists vs ENotDir. No read marker is involved
// (directories have no content freshness), but the §7 lock order still
// applies so a concurrent upload's compare/publish on the same resolved
// path cannot interleave.
func (s *Conn) Mkdir(ctx context.Context, _ *mcp.CallToolRequest, in MkdirInput) (*mcp.CallToolResult, MkdirOutput, error) {
	resolved, err := s.Guard.Validate(in.DirPath)
	if err != nil {
		return nil, MkdirOutput{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, s.UploadLimits.MkdirBudget)
	defer cancel()

	// §7 order: process-level path lock, then session path lock.
	release, err := s.Shared.plocks.LockContext(ctx, resolved)
	if err != nil {
		return nil, MkdirOutput{}, errmsg.ETimeout("mkdir")
	}
	defer release()
	unlock := s.Sess.LockFile(resolved)
	defer unlock()

	status, err := runFSContext(s, ctx, resolved, "mkdir", func() (string, error) {
		return s.mkdirOnce(resolved)
	})
	if err != nil {
		return nil, MkdirOutput{}, mapFSErr(err, resolved)
	}
	return nil, MkdirOutput{DirPath: resolved, Status: status}, nil
}

// mkdirOnce is one §8 attempt: stat first (idempotent exists), then
// Mkdir with the EEXIST race reconciled by a second stat.
func (s *Conn) mkdirOnce(resolved string) (string, error) {
	var status string
	err := s.Roots.WithRoot(resolved, func(root *os.Root, rel string) error {
		info, err := root.Stat(rel)
		if err == nil {
			if info.IsDir() {
				status = "exists"
				return nil
			}
			return errmsg.ENotDir(resolved)
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		// Only the final component may be missing (pathguard proved the
		// parent exists at validation time); a vanished parent is the
		// EParentMissing catalog row, not a retryable backend wobble.
		if err := root.Mkdir(rel, 0o755); err != nil {
			if errors.Is(err, fs.ErrExist) {
				// Lost a create race: reconcile by type.
				info, statErr := root.Stat(rel)
				if statErr == nil && info.IsDir() {
					status = "exists"
					return nil
				}
				if statErr == nil {
					return errmsg.ENotDir(resolved)
				}
				return statErr
			}
			if errors.Is(err, fs.ErrNotExist) {
				return errmsg.EParentMissing(filepath.Dir(resolved))
			}
			return err
		}
		status = "created"
		return nil
	})
	if err != nil {
		return "", err
	}
	return status, nil
}
