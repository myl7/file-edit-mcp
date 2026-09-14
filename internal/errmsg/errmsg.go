// Package errmsg is the project-wide catalog of model-visible error
// messages (ARCHITECTURE §6). It is the ONLY place allowed to compose
// error text shown to the model: every other package classifies errors
// via the sentinel values (errors.Is) and renders them through the E*
// constructors below, filling the §6 table templates verbatim.
//
// The sentinel values themselves are stable identity tokens, never meant
// to be shown to the model directly; the wrapped *Error carries the
// catalog wording.
package errmsg

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Sentinels: one per §6 catalog row. errors.Is(err, ErrXxx) identifies the
// row for control flow (retry policy, MCP error mapping) without matching
// on message text.
var (
	ErrNulByte       = errors.New("ENulByte")
	ErrWindowsPath   = errors.New("EWindowsPath")
	ErrRelative      = errors.New("ERelative")
	ErrOutside       = errors.New("EOutside")
	ErrParentMissing = errors.New("EParentMissing")
	ErrNotExist      = errors.New("ENotExist")
	ErrIsDir         = errors.New("EIsDir")
	ErrNoMatch       = errors.New("ENoMatch")
	ErrAmbiguous     = errors.New("EAmbiguous")
	ErrNoop          = errors.New("ENoop")
	ErrEmptyOld      = errors.New("EEmptyOld")
	ErrStaleRead     = errors.New("EStaleRead")
	ErrBackend       = errors.New("EBackend")
	ErrEditIndex     = errors.New("EEditIndex")
)

// Error is the concrete type every constructor returns. Error() renders
// the §6 catalog wording; Is matches the row's sentinel so callers can
// classify with errors.Is while message text stays centralized here.
type Error struct {
	// sentinel is the catalog row this error reports as (errors.Is target).
	sentinel error
	// msg is the final model-visible message, rendered from the §6 template.
	msg string
	// cause is the optional underlying error (EBackend, EEditIndex); it is
	// unwrappable so errors.Is / errors.As reach the cause's own chain.
	cause error
}

var _ error = (*Error)(nil)

// Error returns the model-visible message.
func (e *Error) Error() string { return e.msg }

// Is reports whether target is this error's catalog sentinel.
func (e *Error) Is(target error) bool { return target == e.sentinel }

// Unwrap exposes the wrapped cause, if any.
func (e *Error) Unwrap() error { return e.cause }

// maxAmbiguousLines is how many line numbers EAmbiguous lists before
// collapsing the rest into "+N more".
const maxAmbiguousLines = 5

// ENulByte reports a NUL byte inside a path (§3 step 1).
func ENulByte() error {
	return &Error{sentinel: ErrNulByte, msg: "path contains a NUL byte"}
}

// EWindowsPath rejects Windows drive-letter paths (§3 step 3). path is the
// (tilde-expanded) raw path; allowedDirs is the allowed-roots list.
func EWindowsPath(path string, allowedDirs []string) error {
	return &Error{sentinel: ErrWindowsPath, msg: fmt.Sprintf(
		"path %q looks like a Windows path; this server uses POSIX absolute paths (allowed: %s)",
		path, FormatDirs(allowedDirs))}
}

// ERelative rejects non-absolute paths (§3 step 4).
func ERelative(allowedDirs []string) error {
	return &Error{sentinel: ErrRelative, msg: fmt.Sprintf(
		"path is not absolute; use an absolute path (allowed: %s)",
		FormatDirs(allowedDirs))}
}

// EOutside rejects a path (or realpath) that lies outside every allowed
// directory (§3 steps 6 and 7).
func EOutside(path string, allowedDirs []string) error {
	return &Error{sentinel: ErrOutside, msg: fmt.Sprintf(
		"path %s is outside the allowed directories: %s",
		path, FormatDirs(allowedDirs))}
}

// EParentMissing reports a missing parent directory for a new file (§3
// step 8; §5.2 write). parent is the cleaned parent path.
func EParentMissing(parent string) error {
	return &Error{sentinel: ErrParentMissing, msg: fmt.Sprintf(
		"parent directory does not exist: %s", parent)}
}

// ENotExist reports a file that does not exist (§5.1 read, §5.3 edit).
func ENotExist(path string) error {
	return &Error{sentinel: ErrNotExist, msg: fmt.Sprintf(
		"file does not exist: %s", path)}
}

// EIsDir reports a path that is a directory, not a file (§5.1).
func EIsDir(path string) error {
	return &Error{sentinel: ErrIsDir, msg: fmt.Sprintf(
		"path is a directory, not a file: %s", path)}
}

// ENoMatch reports zero byte-exact old_string matches (§5.3 edit).
func ENoMatch(path string) error {
	return &Error{sentinel: ErrNoMatch, msg: fmt.Sprintf(
		"old_string not found in %s. Read the file and copy old_string exactly, including all whitespace and indentation. No fuzzy matching is performed.",
		path)}
}

// EAmbiguous reports multiple old_string matches with replace_all=false
// (§5.3): the match count plus at most 5 line numbers, the rest collapsed
// into "+N more".
func EAmbiguous(count int, lines []int, path string) error {
	return &Error{sentinel: ErrAmbiguous, msg: fmt.Sprintf(
		"old_string matches %d times in %s (first at line(s) %s). Include more surrounding context in old_string to make it unique, or set replace_all=true.",
		count, path, FormatLines(lines))}
}

// ENoop rejects an edit whose new_string equals old_string (§5.3).
func ENoop() error {
	return &Error{sentinel: ErrNoop, msg: "old_string is identical to new_string; nothing to do"}
}

// EEmptyOld rejects an empty old_string (§5.3).
func EEmptyOld() error {
	return &Error{sentinel: ErrEmptyOld, msg: "old_string is empty; it would match everywhere. Provide the exact text to replace."}
}

// EStaleRead is the stale variant of the write gate: it rejects modifying a
// file that changed since the caller's last read (§5.2 write, §5.3/§5.4
// edit/multi_edit). The gate's never-read variant is EStaleReadNeverRead;
// both share the ErrStaleRead sentinel, so control flow (retry policy, MCP
// error mapping) never distinguishes them — the model's remedy is identical
// in both cases: read, then write.
func EStaleRead(path string) error {
	return &Error{sentinel: ErrStaleRead, msg: fmt.Sprintf(
		"file changed since last read; read it again before editing: %s", path)}
}

// EStaleReadNeverRead is the never-read variant of the write gate: no
// session marker exists, so any current state counts as
// changed-since-last-known (§5.2 write, §5.3/§5.4 edit/multi_edit). It
// reports the same ErrStaleRead sentinel as EStaleRead —
// errors.Is(err, ErrStaleRead) is true for both variants — because the
// remedy is the same: read the file, then modify it.
func EStaleReadNeverRead(path string) error {
	return &Error{sentinel: ErrStaleRead, msg: fmt.Sprintf(
		"file has not been read in this session; read it before writing: %s", path)}
}

// EBackend reports a CIFS soft-mount backend failure (§8). cause is the
// underlying errno-bearing error; it stays unwrappable so errors.Is can
// reach it.
func EBackend(cause error) error {
	return &Error{sentinel: ErrBackend, cause: cause, msg: fmt.Sprintf(
		"storage backend is unreachable or waking up (CIFS soft mount): %v. Retry shortly; if it persists, check the share server.",
		nonNil(cause))}
}

// EEditIndex is the multi_edit wrapper: it names the failing 1-based edit
// index and embeds the underlying error (§5.4). The inner sentinel stays
// reachable through errors.Is.
func EEditIndex(index int, cause error) error {
	return &Error{sentinel: ErrEditIndex, cause: cause, msg: fmt.Sprintf(
		"edit #%d failed: %v", index, nonNil(cause))}
}

// FormatDirs renders the allowed-roots list for the catalog templates.
// Exported so future packages reuse the exact same rendering.
func FormatDirs(dirs []string) string {
	return strings.Join(dirs, ", ")
}

// FormatLines renders up to maxAmbiguousLines line numbers, then "+N more"
// for the remainder (EAmbiguous placeholder).
func FormatLines(lines []int) string {
	shown, rest := lines, 0
	if len(lines) > maxAmbiguousLines {
		shown, rest = lines[:maxAmbiguousLines], len(lines)-maxAmbiguousLines
	}
	parts := make([]string, 0, len(shown)+1)
	for _, n := range shown {
		parts = append(parts, strconv.Itoa(n))
	}
	if rest > 0 {
		parts = append(parts, "+"+strconv.Itoa(rest)+" more")
	}
	return strings.Join(parts, ", ")
}

// nonNil keeps %v rendering sane if a caller passes a nil cause.
func nonNil(cause error) error {
	if cause == nil {
		return errors.New("unknown error")
	}
	return cause
}
