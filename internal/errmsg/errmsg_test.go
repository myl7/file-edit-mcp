package errmsg

import (
	"errors"
	"syscall"
	"testing"
)

// TestCatalogGoldenMessages asserts the exact model-visible wording of all
// 14 §6 catalog rows (golden snapshots; EStaleRead carries two variant
// wordings under its one row) and that each constructor's error matches
// its sentinel via errors.Is.
func TestCatalogGoldenMessages(t *testing.T) {
	allowed := []string{"/data", "/data/other"}
	cases := []struct {
		name string
		err  error
		want string
		sent error
	}{
		{
			name: "ENulByte",
			err:  ENulByte(),
			want: `path contains a NUL byte`,
			sent: ErrNulByte,
		},
		{
			name: "EWindowsPath",
			err:  EWindowsPath(`C:\Users\x`, allowed),
			want: `path "C:\\Users\\x" looks like a Windows path; this server uses POSIX absolute paths (allowed: /data, /data/other)`,
			sent: ErrWindowsPath,
		},
		{
			name: "ERelative",
			err:  ERelative(allowed),
			want: `path is not absolute; use an absolute path (allowed: /data, /data/other)`,
			sent: ErrRelative,
		},
		{
			name: "EOutside",
			err:  EOutside("/etc/passwd", allowed),
			want: `path /etc/passwd is outside the allowed directories: /data, /data/other`,
			sent: ErrOutside,
		},
		{
			name: "EParentMissing",
			err:  EParentMissing("/data/nodir"),
			want: `parent directory does not exist: /data/nodir`,
			sent: ErrParentMissing,
		},
		{
			name: "ENotExist",
			err:  ENotExist("/data/absent.txt"),
			want: `file does not exist: /data/absent.txt`,
			sent: ErrNotExist,
		},
		{
			name: "EIsDir",
			err:  EIsDir("/data/dir"),
			want: `path is a directory, not a file: /data/dir`,
			sent: ErrIsDir,
		},
		{
			name: "ENoMatch",
			err:  ENoMatch("/data/a.go"),
			want: `old_string not found in /data/a.go. Read the file and copy old_string exactly, including all whitespace and indentation. No fuzzy matching is performed.`,
			sent: ErrNoMatch,
		},
		{
			name: "EAmbiguous",
			err:  EAmbiguous(3, []int{4, 9, 11}, "/data/a.go"),
			want: `old_string matches 3 times in /data/a.go (first at line(s) 4, 9, 11). Include more surrounding context in old_string to make it unique, or set replace_all=true.`,
			sent: ErrAmbiguous,
		},
		{
			name: "ENoop",
			err:  ENoop(),
			want: `old_string is identical to new_string; nothing to do`,
			sent: ErrNoop,
		},
		{
			name: "EEmptyOld",
			err:  EEmptyOld(),
			want: `old_string is empty; it would match everywhere. Provide the exact text to replace.`,
			sent: ErrEmptyOld,
		},
		{
			name: "EStaleReadNeverRead",
			err:  EStaleReadNeverRead("/data/a.go"),
			want: `file has not been read in this session; read it before writing: /data/a.go`,
			sent: ErrStaleRead,
		},
		{
			name: "EStaleRead",
			err:  EStaleRead("/data/a.go"),
			want: `file changed since last read; read it again before editing: /data/a.go`,
			sent: ErrStaleRead,
		},
		{
			name: "EBackend",
			err:  EBackend(errors.New("connection timed out")),
			want: `storage backend is unreachable or waking up (CIFS soft mount): connection timed out. Retry shortly; if it persists, check the share server.`,
			sent: ErrBackend,
		},
		{
			name: "EEditIndex",
			err:  EEditIndex(2, ENoMatch("/data/a.go")),
			want: `edit #2 failed: old_string not found in /data/a.go. Read the file and copy old_string exactly, including all whitespace and indentation. No fuzzy matching is performed.`,
			sent: ErrEditIndex,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.Error(); got != tc.want {
				t.Errorf("message:\n got:  %s\nwant: %s", got, tc.want)
			}
			if !errors.Is(tc.err, tc.sent) {
				t.Errorf("errors.Is(err, sentinel) = false, want true")
			}
		})
	}
}

// TestEAmbiguousLineList exercises the line-list placeholder: at most 5
// line numbers, the rest collapsed into "+N more".
func TestEAmbiguousLineList(t *testing.T) {
	cases := []struct {
		name  string
		lines []int
		want  string
	}{
		{"one", []int{7}, `7`},
		{"exactly five", []int{1, 2, 3, 4, 5}, `1, 2, 3, 4, 5`},
		{"six collapses", []int{1, 2, 3, 4, 5, 6}, `1, 2, 3, 4, 5, +1 more`},
		{"nine collapses", []int{2, 4, 6, 8, 10, 12, 14, 16, 18}, `2, 4, 6, 8, 10, +4 more`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := FormatLines(tc.lines)
			if got != tc.want {
				t.Errorf("FormatLines(%v) = %q, want %q", tc.lines, got, tc.want)
			}
		})
	}
}

// TestEAmbiguousTruncatedGolden is the full-message golden for the
// truncated line list.
func TestEAmbiguousTruncatedGolden(t *testing.T) {
	err := EAmbiguous(9, []int{2, 4, 6, 8, 10, 12, 14, 16, 18}, "/f.go")
	want := `old_string matches 9 times in /f.go (first at line(s) 2, 4, 6, 8, 10, +4 more). Include more surrounding context in old_string to make it unique, or set replace_all=true.`
	if err.Error() != want {
		t.Errorf("message:\n got:  %s\nwant: %s", err.Error(), want)
	}
}

// TestEBackendCauseChain: the sentinel matches AND the underlying errno
// stays reachable through errors.Is (retry classification depends on it).
func TestEBackendCauseChain(t *testing.T) {
	err := EBackend(syscall.EIO)
	if !errors.Is(err, ErrBackend) {
		t.Fatal("errors.Is(err, ErrBackend) = false")
	}
	if !errors.Is(err, syscall.EIO) {
		t.Error("errors.Is(err, syscall.EIO) = false, want the cause unwrappable")
	}
	if err.Error() != "storage backend is unreachable or waking up (CIFS soft mount): input/output error. Retry shortly; if it persists, check the share server." {
		t.Errorf("unexpected message: %s", err.Error())
	}
}

// TestEBackendNilCause guards against a nil cause rendering as %!v(<nil>).
func TestEBackendNilCause(t *testing.T) {
	err := EBackend(nil)
	if !errors.Is(err, ErrBackend) {
		t.Fatal("errors.Is(err, ErrBackend) = false")
	}
	if got, want := err.Error(), "storage backend is unreachable or waking up (CIFS soft mount): unknown error. Retry shortly; if it persists, check the share server."; got != want {
		t.Errorf("message:\n got:  %s\nwant: %s", got, want)
	}
}

// TestEEditIndexCauseChain: both the wrapper sentinel and the inner row
// sentinel must be identifiable from the wrapped error alone.
func TestEEditIndexCauseChain(t *testing.T) {
	err := EEditIndex(3, ENoMatch("/f.go"))
	if !errors.Is(err, ErrEditIndex) {
		t.Error("errors.Is(err, ErrEditIndex) = false")
	}
	if !errors.Is(err, ErrNoMatch) {
		t.Error("errors.Is(err, ErrNoMatch) = false, want the inner sentinel reachable")
	}
}

// TestEStaleReadVariantsShareSentinel pins the merged write gate: the
// never-read and the stale wording both report the ONE ErrStaleRead
// sentinel (errors.Is is true for both — control flow must not care which
// variant fired), while the messages stay distinct so the model sees which
// remedy applies: read first, or re-read.
func TestEStaleReadVariantsShareSentinel(t *testing.T) {
	unread := EStaleReadNeverRead("/data/a.go")
	stale := EStaleRead("/data/a.go")
	if !errors.Is(unread, ErrStaleRead) {
		t.Error("errors.Is(never-read variant, ErrStaleRead) = false, want true")
	}
	if !errors.Is(stale, ErrStaleRead) {
		t.Error("errors.Is(stale variant, ErrStaleRead) = false, want true")
	}
	if unread.Error() == stale.Error() {
		t.Errorf("never-read and stale variants render identically: %q", unread.Error())
	}
}

// TestSentinelsDistinct ensures no two catalog rows share an identity.
func TestSentinelsDistinct(t *testing.T) {
	all := []error{
		ErrNulByte, ErrWindowsPath, ErrRelative, ErrOutside, ErrParentMissing,
		ErrNotExist, ErrIsDir, ErrNoMatch, ErrAmbiguous, ErrNoop, ErrEmptyOld,
		ErrStaleRead, ErrBackend, ErrEditIndex,
	}
	seen := make(map[error]bool, len(all))
	for _, s := range all {
		if seen[s] {
			t.Errorf("duplicate sentinel %v", s)
		}
		seen[s] = true
	}
}

// TestGoldenErrorType ensures constructors return *Error so callers can
// rely on the type (and never on sentinel text reaching the model).
func TestGoldenErrorType(t *testing.T) {
	var e *Error
	if err := ENoop(); !errors.As(err, &e) {
		t.Error("ENoop does not return *Error")
	}
}
