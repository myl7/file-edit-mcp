// read tool handler (ARCHITECTURE §5.1): pathguard → Root/Retry read →
// lineio.Render → session MarkRead.

package tools

import (
	"context"
	"os"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/myl7/file-edit-mcp/internal/errmsg"
	"github.com/myl7/file-edit-mcp/internal/lineio"
)

// Read implements the read tool (§5.1): numbered lines "<line>\t<content>",
// the three output limits, and the continuation footer whenever the read
// did not reach EOF. A successful read records the session marker so later
// writes/edits on the same path pass the read-first check.
func (s *Conn) Read(ctx context.Context, _ *mcp.CallToolRequest, in ReadInput) (*mcp.CallToolResult, ReadOutput, error) {
	// §5.1: offset defaults to 1, limit to DefaultLines (2000). The zero
	// value is the "absent" signal; explicitly negative values are argument
	// errors caught by ValidateArgs.
	offset, limit := in.Offset, in.Limit
	if offset == 0 {
		offset = 1
	}
	if limit == 0 {
		limit = lineio.DefaultLines
	}
	if err := lineio.ValidateArgs(offset, limit); err != nil {
		return nil, ReadOutput{}, err
	}

	resolved, err := s.Guard.Validate(in.FilePath)
	if err != nil {
		return nil, ReadOutput{}, err
	}

	// §7 order: the process-level path lock first (§5.9 added it so an
	// upload's compare/publish cannot interleave with a read of the same
	// resolved path across tokens), then the per-path session lock so the
	// marker we record describes a quiescent file.
	release, err := s.Shared.plocks.LockContext(ctx, resolved)
	if err != nil {
		return nil, ReadOutput{}, errmsg.ETimeout("read")
	}
	defer release()
	unlock := s.Sess.LockFile(resolved)
	defer unlock()

	type readSnap struct {
		content []byte
		size    int64
		mtime   time.Time
	}
	// The stat runs BEFORE the content read: if the file changes in between,
	// the recorded marker is the pre-change state and any later edit on the
	// post-change file detects EStaleRead instead of silently proceeding.
	snap, err := runFS(s, resolved, func() (*readSnap, error) {
		var out *readSnap
		err := s.Roots.WithRoot(resolved, func(root *os.Root, rel string) error {
			info, err := root.Stat(rel)
			if err != nil {
				return err
			}
			if info.IsDir() {
				return errmsg.EIsDir(resolved)
			}
			data, err := root.ReadFile(rel)
			if err != nil {
				return err
			}
			out = &readSnap{content: data, size: info.Size(), mtime: info.ModTime()}
			return nil
		})
		return out, err
	})
	if err != nil {
		return nil, ReadOutput{}, mapFSErr(err, resolved)
	}

	// §5.1: rendering is the display layer — UTF-8 sanitization and the
	// truncation limits happen here, never on the stored bytes.
	res, err := lineio.Render(snap.content, offset, limit)
	if err != nil {
		return nil, ReadOutput{}, err
	}

	s.Sess.MarkRead(resolved, snap.size, snap.mtime)
	return nil, ReadOutput{Content: res.Text, Footer: res.Footer}, nil
}
