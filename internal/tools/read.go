// read tool handler (ARCHITECTURE §5.1): pathguard → Root/Retry read →
// lineio.Render → session MarkRead.

package tools

import (
	"context"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/myl7/file-edit-mcp/internal/errmsg"
	"github.com/myl7/file-edit-mcp/internal/lineio"
)

// Read implements the read tool (§5.1): numbered lines "<line>\t<content>",
// the three output limits, and the continuation footer whenever the read
// did not reach EOF. A successful read records the session marker so later
// writes/edits on the same path pass the read-first check.
func (s *Conn) Read(_ context.Context, _ *mcp.CallToolRequest, in ReadInput) (*mcp.CallToolResult, ReadOutput, error) {
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

	// §7: serialize with writers on the same path so the marker we record
	// describes a quiescent file (no torn read across a concurrent rename).
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
		root, rel, err := s.Roots.Resolve(resolved)
		if err != nil {
			return nil, err
		}
		info, err := root.Stat(rel)
		if err != nil {
			return nil, err
		}
		if info.IsDir() {
			return nil, errmsg.EIsDir(resolved)
		}
		data, err := root.ReadFile(rel)
		if err != nil {
			return nil, err
		}
		return &readSnap{content: data, size: info.Size(), mtime: info.ModTime()}, nil
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
