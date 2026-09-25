// edit and multi_edit tool handlers (ARCHITECTURE §5.3/§5.4): read-first
// and stale-read checks, the pure editengine application, a pre-write
// mtime/size re-check, then the shared atomic write path.

package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/myl7/file-edit-mcp/internal/contentwarn"
	"github.com/myl7/file-edit-mcp/internal/editengine"
	"github.com/myl7/file-edit-mcp/internal/errmsg"
	"github.com/myl7/file-edit-mcp/internal/session"
)

// editAck is the (informal) structured output of edit/multi_edit; §5
// specifies no output shape except the §5.7 warning field below, so Out
// stays `any`.
type editAck struct {
	// Matches counts replaced occurrences (edit) or applied edits
	// (multi_edit).
	Matches int `json:"matches"`
	// Warning is the §5.7 content warning: non-empty when the scanned
	// new_string text contained suspicious control characters. The edit
	// itself is never affected.
	Warning string `json:"warning,omitempty"`
}

// Edit implements the edit tool (§5.3): one byte-exact replacement with
// uniqueness required unless replace_all. Engine errors (ENoMatch,
// EAmbiguous, ENoop, EEmptyOld) pass through with the §6 catalog text; the
// resolved path is attached because the engine itself is path-free.
func (s *Conn) Edit(_ context.Context, _ *mcp.CallToolRequest, in EditInput) (*mcp.CallToolResult, any, error) {
	resolved, err := s.Guard.Validate(in.FilePath)
	if err != nil {
		return nil, nil, err
	}

	unlock := s.Sess.LockFile(resolved)
	defer unlock()

	matches := 0
	err = s.editFlow(resolved, func(content []byte) ([]byte, error) {
		// §5.3: matching is byte-exact against the raw file bytes — no
		// UTF-8 normalization happens here (that is read's display layer).
		out, n, err := editengine.ApplyOnce(content, []byte(in.OldString), []byte(in.NewString), in.ReplaceAll)
		if err != nil {
			return nil, withPath(err, resolved)
		}
		matches = n
		return out, nil
	})
	if err != nil {
		return nil, nil, err
	}
	// §5.7: on the success path only (errors above keep their §6 catalog
	// surface), scan the received new_string — old_string comes from the
	// already-read file and is deliberately not scanned (§5.7). The edit
	// itself is untouched either way.
	warning := contentwarn.Render("new_string", contentwarn.Check(in.NewString))
	if warning != "" {
		s.log().Warn("suspicious content in edit", "path", resolved, "warning", warning)
	}
	return nil, any(editAck{Matches: matches, Warning: warning}), nil
}

// MultiEdit implements the multi_edit tool (§5.4): edits applied in array
// order onto progressively modified content; any failure names the 1-based
// edit index (the engine's EEditIndex wrapper passes through verbatim) and
// nothing is written.
func (s *Conn) MultiEdit(_ context.Context, _ *mcp.CallToolRequest, in MultiEditInput) (*mcp.CallToolResult, any, error) {
	resolved, err := s.Guard.Validate(in.FilePath)
	if err != nil {
		return nil, nil, err
	}
	if len(in.Edits) == 0 {
		return nil, nil, errors.New("edits must contain at least one edit")
	}

	unlock := s.Sess.LockFile(resolved)
	defer unlock()

	err = s.editFlow(resolved, func(content []byte) ([]byte, error) {
		edits := make([]editengine.Edit, len(in.Edits))
		for i, e := range in.Edits {
			edits[i] = editengine.Edit{
				Old:        []byte(e.OldString),
				New:        []byte(e.NewString),
				ReplaceAll: e.ReplaceAll,
			}
		}
		// ApplyMany wraps any per-edit failure as EEditIndex (§5.4); the
		// error already carries the index and the §6 cause text.
		return editengine.ApplyMany(content, edits)
	})
	if err != nil {
		return nil, nil, err
	}
	// §5.7: on the success path only (errors keep their §6 catalog surface,
	// including EEditIndex for edit #i), scan each edit's new_string — never
	// old_string — and attribute findings to the 1-based edit index; the
	// parts join into the single ack warning and the edits above are
	// untouched either way.
	var parts []string
	for i, e := range in.Edits {
		if w := contentwarn.Render(fmt.Sprintf("edit #%d new_string", i+1), contentwarn.Check(e.NewString)); w != "" {
			parts = append(parts, w)
		}
	}
	warning := strings.Join(parts, "; ")
	if warning != "" {
		s.log().Warn("suspicious content in multi_edit", "path", resolved, "warning", warning)
	}
	return nil, any(editAck{Matches: len(in.Edits), Warning: warning}), nil
}

// editFlow is the shared §5.3/§5.4 sequence for one resolved path (the
// caller holds the per-path session lock); write.go's Write runs the same
// fence at steps 2 and 4 for existing targets:
//
//  1. stat + read the current bytes (ENOENT → ENotExist, directory →
//     EIsDir);
//  2. compare against the session marker — never read → the never-read
//     EStaleRead variant, changed since read → the stale variant (one
//     sentinel, one remedy: read, then write);
//  3. apply the caller's pure edit function in memory (all-or-nothing);
//  4. re-stat: if mtime/size moved since step 1, the file changed under us
//     → EStaleRead, nothing written;
//  5. atomic temp+rename write (§5.2 path), then refresh the marker — the
//     new content is known.
func (s *Conn) editFlow(resolved string, apply func(content []byte) ([]byte, error)) error {
	type snap struct {
		content []byte
		size    int64
		mtime   time.Time
	}
	cur, err := runFS(s, resolved, func() (*snap, error) {
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
		return &snap{content: data, size: info.Size(), mtime: info.ModTime()}, nil
	})
	if err != nil {
		return mapFSErr(err, resolved)
	}

	switch s.Sess.Compare(resolved, cur.size, cur.mtime) {
	case session.StatusUnknown:
		return errmsg.EStaleReadNeverRead(resolved)
	case session.StatusStale:
		return errmsg.EStaleRead(resolved)
	}

	// In-memory all-or-nothing application (§5.3): only a fully applied
	// result ever reaches the write below.
	out, err := apply(cur.content)
	if err != nil {
		return err
	}

	// Pre-write re-check (§5.3): anything that changed the file since the
	// state the marker matched makes this EStaleRead instead of a silent
	// overwrite of a concurrent writer's work.
	size, mtime, mode, err := s.statFile(resolved)
	if err != nil {
		return mapFSErr(err, resolved)
	}
	if size != cur.size || !mtime.Equal(cur.mtime) {
		return errmsg.EStaleRead(resolved)
	}

	if err := s.atomicWrite(resolved, out, mode); err != nil {
		return mapFSErr(err, resolved)
	}

	s.markKnown(resolved, int64(len(out)))
	return nil
}
