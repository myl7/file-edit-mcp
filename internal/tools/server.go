// Package tools hosts the six MCP tool handlers (the assembly layer over
// pathguard/editengine/lineio/session/cifsops, per ARCHITECTURE §1).
//
// read/write/edit/multi_edit are implemented here (T6); glob and grep here
// too (T7). The file-mutating handlers follow the same shape: validate the
// path through the Guard (§3), take the per-path session lock (§7), then run
// all filesystem work through runFS so backend-class errors retry per §8 and
// surface with the EBackend catalog wording. glob/grep are read-only
// traversals: they validate their search roots through the Guard but walk
// with plain WalkDir (§4 waives the os.Root requirement for them).
//
// Handlers return plain errors; the MCP SDK wraps them into IsError tool
// results with the error text as content, which is exactly the §6 contract
// (catalog text, nothing else).
package tools

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/myl7/file-edit-mcp/internal/cifsops"
	"github.com/myl7/file-edit-mcp/internal/errmsg"
	"github.com/myl7/file-edit-mcp/internal/pathguard"
	"github.com/myl7/file-edit-mcp/internal/session"
)

// Shared carries the connection-independent, process-level state: the §3
// path Guard, the §4 Root pool, and the §8 Retrier. One Shared instance
// serves every MCP connection of the process (stdio: the single connection;
// HTTP: all StreamableHTTP sessions, ARCHITECTURE §0/§7). All fields are
// set once at startup and are safe for concurrent use: Guard is immutable
// after construction, RootPool is documented thread-safe, and the Retrier
// guards its state internally (its "cold" flag is deliberately process-wide:
// one successful fs operation anywhere warms the whole process, matching the
// one-automount-per-share reality underneath). Create with NewShared.
type Shared struct {
	// Guard is the §3 path validation pipeline.
	Guard *pathguard.Guard
	// Retry drives the §8 CIFS retry policy; its OnRetry hook reopens the
	// Root pool to re-trigger the automount.
	Retry *cifsops.Retrier
	// Roots is the lazily-opened os.Root pool (§4).
	Roots *cifsops.RootPool
	// Log writes to stderr only; stdout is reserved for MCP frames (§7).
	Log *slog.Logger

	// rename is os.Root.Rename; an unexported field so the atomicity tests
	// (§9.9) can inject a rename failure. Set by NewShared, never nil.
	rename func(r *os.Root, oldname, newname string) error
}

// NewShared validates the allowed directories (pathguard canonicalization;
// a missing or non-directory --allow entry is a startup error), builds the
// Root pool over the canonicalized list, and wires the Retrier's OnRetry
// hook to reopen the pool. log may be nil (then slog.Default is used).
func NewShared(allowedDirs []string, log *slog.Logger) (*Shared, error) {
	guard, err := pathguard.New(allowedDirs)
	if err != nil {
		return nil, err
	}
	roots := cifsops.NewRootPool(guard.AllowedDirs())
	s := &Shared{
		Guard: guard,
		Roots: roots,
		Retry: &cifsops.Retrier{
			// §8: before every backend retry, close and reopen the Roots so
			// the automount remounts the share.
			OnRetry: func(int) error { return roots.ReopenAll() },
		},
		Log: log,
	}
	s.rename = func(r *os.Root, oldname, newname string) error {
		return r.Rename(oldname, newname)
	}
	return s, nil
}

// Conn is the read-marker and per-path-lock state (§7) layered over the
// process-wide Shared state. Scope is per transport (§0): stdio builds
// exactly one — one process is one session anyway — and the stateless HTTP
// transport builds ONE PER TOKEN for the lifetime of the HTTP handler
// (token-as-session: the token in /{token}/mcp is the session identity),
// registering it on every per-request *mcp.Server that route's
// StreamableHTTP GetServer callback constructs. Marker semantics follow
// that scope: the write gate EStaleRead is PER TOKEN — its never-read
// variant means a file read under token A does not authorize a write under
// token B (the cross-client isolation the stateless 2026-07-28 protocol,
// SEP-2567, removed is restored along the one axis that protocol leaves
// standing, the credential) — while the stale variant compares against the
// file's CURRENT state, so ANY writer — another token or an out-of-band
// disk change — stales every other token's marker for that path. Within
// one token the markers persist across stateless requests (statelessness
// carries no sessions, but the Conn does) and reset on process restart.
// Cross-token same-file concurrency is safe without a shared lock: each
// write is an atomic temp+rename, and the pre-write re-stat shared by
// editFlow and Write turns the loser of a race into EStaleRead, never a
// silent overwrite; same-path writes under ONE token stay serialized by
// that token's per-path lock table.
//
// Concurrent use by overlapping requests is safe: Session guards markers
// and locks behind one mutex (verified with -race), and Register is pure
// wiring (see below), so any number of per-request servers may share one
// Conn simultaneously.
type Conn struct {
	// *Shared embeds the process-level Guard/Retry/Roots/Log.
	*Shared
	// Sess is this connection's read marker and per-path lock store (§7).
	Sess *session.Session
}

// NewConn returns a fresh connection state over s: an empty session, sharing
// s's Guard, Root pool, and Retrier. stdio calls it once per process; the
// stateless HTTP transport calls it once per token for the HTTP handler's
// lifetime and registers the result on every per-request server of that
// token's route (see Conn for the per-token marker scope that follows).
// Safe to call concurrently.
func (s *Shared) NewConn() *Conn {
	return &Conn{Shared: s, Sess: session.New()}
}

// Register adds all six tools to srv: read/write/edit/multi_edit (files
// read.go/write.go/edit.go) and glob/grep (files glob.go/grep.go).
//
// Pure wiring: it hands six bound handler method values to mcp.AddTool and
// keeps no state on the Conn, so registering one Conn on any number of
// servers — the stateless HTTP handler does exactly that, once per
// per-request server — is safe, concurrently callable, and idempotent
// (Server.AddTool replaces a same-name tool rather than duplicating it).
// The marker/lock state the handlers close over lives in the Conn, shared
// by every server the Conn is registered on; see the Conn doc for that
// scope.
func (s *Conn) Register(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{Name: "read", Description: toolDescRead}, s.Read)
	mcp.AddTool(srv, &mcp.Tool{Name: "write", Description: toolDescWrite}, s.Write)
	mcp.AddTool(srv, &mcp.Tool{Name: "edit", Description: toolDescEdit}, s.Edit)
	mcp.AddTool(srv, &mcp.Tool{Name: "multi_edit", Description: toolDescMultiEdit}, s.MultiEdit)
	mcp.AddTool(srv, &mcp.Tool{Name: "glob", Description: toolDescGlob}, s.Glob)
	mcp.AddTool(srv, &mcp.Tool{Name: "grep", Description: toolDescGrep}, s.Grep)
}

// Keepalive starts the §0 automount keepalive loop (HTTP mode): every
// interval it stats each allowed directory through the Root pool so the
// CIFS automount never sees the share idle long enough to unmount. An
// unmounted share makes stat return ENOENT, which §8 classifies as
// definitive (never retried) — a tool call would then misreport "file does
// not exist" for a share that is merely not mounted; this loop keeps the
// mount busy so that cannot happen (idle-timeout 600s vs the 4m default).
//
// interval <= 0 disables the loop entirely (--keepalive 0). probe is the
// per-directory stat; nil uses the production probe (RootPool resolve +
// Root.Stat), and tests inject a counting/failing probe. Failures are
// logged at warn level and never abort the loop: the loop must outlive
// transient mount trouble (and a genuinely broken mount surfaces through
// the §8 backend retry path on real tool calls). The goroutine exits when
// ctx is done.
func (s *Shared) Keepalive(ctx context.Context, interval time.Duration, probe func(dir string) error) {
	if interval <= 0 {
		return
	}
	if probe == nil {
		probe = func(dir string) error {
			root, rel, err := s.Roots.Resolve(dir)
			if err != nil {
				return err
			}
			_, err = root.Stat(rel)
			return err
		}
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				for _, dir := range s.Guard.AllowedDirs() {
					if err := probe(dir); err != nil {
						s.log().Warn("keepalive stat failed", "dir", dir, "err", err)
					}
				}
			}
		}
	}()
}

// runFS executes one filesystem operation under the §8 retry policy: op
// runs inside Retrier.Do (so backend-class errors retrigger the mount via
// the OnRetry hook and are retried), and a final backend-class error is
// wrapped into the EBackend catalog message (§6). path is used for logging
// context only; op itself resolves the Root and relative name.
//
// Handlers wrap their whole multi-step fs sequence in one runFS call where
// practical so a mid-sequence backend error retries the sequence cleanly.
func runFS[T any](s *Conn, path string, op func() (T, error)) (T, error) {
	var out T
	err := s.Retry.Do(func() error {
		v, err := op()
		if err != nil {
			return err
		}
		out = v
		return nil
	})
	if err != nil {
		if cifsops.Classify(err) == cifsops.ClassBackend {
			s.log().Debug("fs operation failed after retries", "path", path, "err", err)
			return out, errmsg.EBackend(err)
		}
		return out, err
	}
	return out, nil
}

// log returns the configured logger or the default one.
func (s *Shared) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// mapFSErr translates raw definite-class os errors into §6 catalog entries
// (ENOENT → ENotExist, EISDIR → EIsDir, ENOTDIR → ENotExist, matching the
// pathguard convention). Catalog errors and backend-wrapped errors pass
// through untouched; anything else is returned as-is (there is no catalog
// row for it, and the os error text is the best available wording).
func mapFSErr(err error, path string) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return errmsg.ENotExist(path)
	case errors.Is(err, syscall.EISDIR):
		return errmsg.EIsDir(path)
	case errors.Is(err, syscall.ENOTDIR):
		return errmsg.ENotExist(path)
	default:
		return err
	}
}

// withPath re-attaches the resolved file path to editengine errors. The
// engine is deliberately path-free (§1: "the file" in its messages), so the
// assembly layer appends the path context here: the %w wrap keeps the §6
// catalog text authoritative and the sentinel reachable for errors.Is.
func withPath(err error, path string) error {
	return fmt.Errorf("%w (file: %s)", err, path)
}
