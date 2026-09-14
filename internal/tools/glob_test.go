// glob tool tests (ARCHITECTURE §5.5 / §9 matrix rows 11): direct handler
// calls over a temp allowed dir — mtime ordering with os.Chtimes-controlled
// times, doublestar depth semantics, path narrowing/validation, the two
// safety notes, and the permission-skip behavior.
//
// The limit tests temporarily override the globMaxResults/globScanLimit
// package vars. They must therefore not use t.Parallel — package tests run
// serially unless marked parallel, and nothing else reads those vars
// concurrently.

package tools

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newDirectServer builds a Conn over a fresh temp allowed dir for direct
// handler invocation (no MCP client hop); returns the canonicalized dir.
func newDirectServer(t *testing.T) (*Conn, string) {
	t.Helper()
	dir := t.TempDir()
	sh, err := NewShared([]string{dir}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewShared: %v", err)
	}
	s := sh.NewConn()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", dir, err)
	}
	return s, real
}

// callGlob invokes the Glob handler directly and fails on error.
func callGlob(t *testing.T, s *Conn, in GlobInput) GlobOutput {
	t.Helper()
	_, out, err := s.Glob(context.Background(), nil, in)
	if err != nil {
		t.Fatalf("glob %+v: %v", in, err)
	}
	return out
}

// globErr invokes the Glob handler directly and asserts it fails with every
// substring present.
func globErr(t *testing.T, s *Conn, in GlobInput, substrs ...string) {
	t.Helper()
	_, _, err := s.Glob(context.Background(), nil, in)
	if err == nil {
		t.Fatalf("glob %+v: expected error, got success", in)
	}
	for _, sub := range substrs {
		if !strings.Contains(err.Error(), sub) {
			t.Errorf("glob error %q missing substring %q", err, sub)
		}
	}
}

// setMtime stamps an explicit mtime so ordering is deterministic.
func setMtime(t *testing.T, path string, mtime time.Time) {
	t.Helper()
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("Chtimes(%s): %v", path, err)
	}
}

// globBaseTime anchors the fabricated mtimes (stable, in the past).
var globBaseTime = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

func TestGlobMtimeOrderNewestFirst(t *testing.T) {
	s, dir := newDirectServer(t)
	// Files created in name order but stamped in reverse: newest must be
	// first regardless of creation or lexical order.
	stamps := map[string]time.Duration{
		"a.txt": 3 * time.Hour,
		"b.txt": 1 * time.Hour,
		"c.txt": 2 * time.Hour,
	}
	for name := range stamps {
		writeDisk(t, filepath.Join(dir, name), "x\n", 0o644)
	}
	for name, d := range stamps {
		setMtime(t, filepath.Join(dir, name), globBaseTime.Add(d))
	}

	out := callGlob(t, s, GlobInput{Pattern: "*.txt"})
	want := []string{filepath.Join(dir, "a.txt"), filepath.Join(dir, "c.txt"), filepath.Join(dir, "b.txt")}
	if len(out.Files) != len(want) {
		t.Fatalf("Files = %v, want %v", out.Files, want)
	}
	for i := range want {
		if out.Files[i] != want[i] {
			t.Errorf("Files[%d] = %s, want %s (mtime desc ordering)", i, out.Files[i], want[i])
		}
	}
	if out.Note != "" {
		t.Errorf("Note = %q, want empty", out.Note)
	}
}

func TestGlobDirectoriesNeverMatch(t *testing.T) {
	s, dir := newDirectServer(t)
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeDisk(t, filepath.Join(dir, "sub", "f.txt"), "x\n", 0o644)

	// "**" must list the file but never the directory itself.
	out := callGlob(t, s, GlobInput{Pattern: "**"})
	if len(out.Files) != 1 || out.Files[0] != filepath.Join(dir, "sub", "f.txt") {
		t.Errorf("Files = %v, want exactly [sub/f.txt]; directories are not matches", out.Files)
	}
}

func TestGlobDoublestarDepthSemantics(t *testing.T) {
	s, dir := newDirectServer(t)
	writeDisk(t, filepath.Join(dir, "top.txt"), "x\n", 0o644)
	if err := os.MkdirAll(filepath.Join(dir, "sub", "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeDisk(t, filepath.Join(dir, "sub", "mid.txt"), "x\n", 0o644)
	writeDisk(t, filepath.Join(dir, "sub", "deep", "low.txt"), "x\n", 0o644)
	writeDisk(t, filepath.Join(dir, "top.dat"), "x\n", 0o644)

	for _, tc := range []struct {
		pattern string
		want    []string
	}{
		// "**" crosses directories at any depth INCLUDING zero: top-level
		// files and nested files both match (§5.5).
		{"**", []string{"top.dat", "top.txt", "sub/mid.txt", "sub/deep/low.txt"}},
		// A bare "*" stays at the root level only.
		{"*.txt", []string{"top.txt"}},
		// "**/" may consume zero directories, so **/*.txt includes top.txt.
		{"**/*.txt", []string{"top.txt", "sub/mid.txt", "sub/deep/low.txt"}},
		{"sub/**/*.txt", []string{"sub/mid.txt", "sub/deep/low.txt"}},
		{"**/deep/*.txt", []string{"sub/deep/low.txt"}},
	} {
		out := callGlob(t, s, GlobInput{Pattern: tc.pattern})
		if len(out.Files) != len(tc.want) {
			t.Errorf("pattern %q: Files = %v, want %d entries", tc.pattern, out.Files, len(tc.want))
			continue
		}
		got := make(map[string]bool, len(out.Files))
		for _, f := range out.Files {
			rel, err := filepath.Rel(dir, f)
			if err != nil {
				t.Fatal(err)
			}
			got[filepath.ToSlash(rel)] = true
		}
		for _, w := range tc.want {
			if !got[w] {
				t.Errorf("pattern %q: missing %q in Files %v", tc.pattern, w, out.Files)
			}
		}
	}
}

func TestGlobPathNarrowsRoot(t *testing.T) {
	s, dir := newDirectServer(t)
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeDisk(t, filepath.Join(dir, "root.txt"), "x\n", 0o644)
	writeDisk(t, filepath.Join(dir, "sub", "inner.txt"), "x\n", 0o644)

	// Patterns are relative to the given path, not the allowed root.
	out := callGlob(t, s, GlobInput{Pattern: "*.txt", Path: filepath.Join(dir, "sub")})
	if len(out.Files) != 1 || out.Files[0] != filepath.Join(dir, "sub", "inner.txt") {
		t.Errorf("Files = %v, want [sub/inner.txt] only (path narrows the root)", out.Files)
	}
	// Without path, both are in scope ("**/" spans zero and one level).
	out = callGlob(t, s, GlobInput{Pattern: "**/*.txt"})
	if len(out.Files) != 2 {
		t.Errorf("Files = %v, want root.txt and sub/inner.txt", out.Files)
	}
}

func TestGlobPathAndPatternValidation(t *testing.T) {
	s, dir := newDirectServer(t)
	writeDisk(t, filepath.Join(dir, "plain.txt"), "x\n", 0o644)

	globErr(t, s, GlobInput{Pattern: ""}, "pattern must not be empty")
	globErr(t, s, GlobInput{Pattern: "[oops"}, "invalid doublestar pattern")
	// A file is not a valid search root (§5.5: path is a search START point).
	globErr(t, s, GlobInput{Pattern: "*", Path: filepath.Join(dir, "plain.txt")},
		"must be a directory, not a file")
	globErr(t, s, GlobInput{Pattern: "*", Path: "/etc"}, "outside the allowed directories")
	// Missing directory under an allowed root: the pathguard new-file branch
	// lets it through, the existence check does not.
	globErr(t, s, GlobInput{Pattern: "*", Path: filepath.Join(dir, "gone")},
		"file does not exist")
}

func TestGlobNoMatchIsNotEmptyError(t *testing.T) {
	s, dir := newDirectServer(t)
	writeDisk(t, filepath.Join(dir, "a.txt"), "x\n", 0o644)

	out := callGlob(t, s, GlobInput{Pattern: "*.md"})
	if len(out.Files) != 0 {
		t.Errorf("Files = %v, want empty for a non-matching pattern", out.Files)
	}
	if out.Note != "" {
		t.Errorf("Note = %q, want empty", out.Note)
	}
}

func TestGlobResultLimitTruncationNote(t *testing.T) {
	restore := overrideInt(&globMaxResults, 3)
	defer restore()
	s, dir := newDirectServer(t)
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		p := filepath.Join(dir, name+".txt")
		writeDisk(t, p, "x\n", 0o644)
		setMtime(t, p, globBaseTime.Add(time.Duration(name[0])*time.Minute))
	}

	out := callGlob(t, s, GlobInput{Pattern: "*.txt"})
	if len(out.Files) != 3 {
		t.Fatalf("Files = %v, want 3 entries (cap)", out.Files)
	}
	want := "showing 3 of 5 matches (limit 3); narrow the pattern or set path to a subdirectory"
	if out.Note != want {
		t.Errorf("Note = %q, want %q", out.Note, want)
	}
	// The kept entries are the newest three (mtime desc still holds).
	if !strings.HasSuffix(out.Files[0], "e.txt") {
		t.Errorf("Files[0] = %s, want the newest file first after truncation", out.Files[0])
	}
}

func TestGlobScanLimitStopsTraversal(t *testing.T) {
	restoreScan := overrideInt(&globScanLimit, 4)
	defer restoreScan()
	s, dir := newDirectServer(t)
	for _, name := range []string{"a", "b", "c"} {
		writeDisk(t, filepath.Join(dir, name+".txt"), "x\n", 0o644)
	}
	if err := os.Mkdir(filepath.Join(dir, "zz"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeDisk(t, filepath.Join(dir, "zz", "never.txt"), "x\n", 0o644)

	// Lexical walk order: root(1) a(2) b(3) c(4) — the valve fires at entry
	// 4, so zz/ is never traversed.
	out := callGlob(t, s, GlobInput{Pattern: "**"})
	for _, f := range out.Files {
		if strings.HasSuffix(f, "never.txt") {
			t.Errorf("Files = %v: entry beyond the scan valve was traversed", out.Files)
		}
	}
	want := "scan stopped after 4 entries; results may be incomplete"
	if out.Note != want {
		t.Errorf("Note = %q, want %q", out.Note, want)
	}
}

func TestGlobBothNotesJoined(t *testing.T) {
	restoreMax := overrideInt(&globMaxResults, 2)
	defer restoreMax()
	// The valve counts the root dir too: root(1) a(2) b(3) c(4) — all three
	// files are visited before the scan stops, so 3 matches hit the cap of 2.
	restoreScan := overrideInt(&globScanLimit, 4)
	defer restoreScan()
	s, dir := newDirectServer(t)
	for _, name := range []string{"a", "b", "c"} {
		p := filepath.Join(dir, name+".txt")
		writeDisk(t, p, "x\n", 0o644)
		setMtime(t, p, globBaseTime.Add(time.Duration(name[0])*time.Minute))
	}

	out := callGlob(t, s, GlobInput{Pattern: "*.txt"})
	if len(out.Files) != 2 {
		t.Fatalf("Files = %v, want 2 (cap)", out.Files)
	}
	want := "showing 2 of 3 matches (limit 2); narrow the pattern or set path to a subdirectory; " +
		"scan stopped after 4 entries; results may be incomplete"
	if out.Note != want {
		t.Errorf("Note = %q, want %q (both notes joined by '; ')", out.Note, want)
	}
}

func TestGlobPermissionErrorSkippedNotFailed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: chmod 0 cannot produce EACCES")
	}
	s, dir := newDirectServer(t)
	writeDisk(t, filepath.Join(dir, "ok.txt"), "x\n", 0o644)
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	writeDisk(t, filepath.Join(locked, "secret.txt"), "x\n", 0o644)
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	// The unreadable directory is skipped (and logged to stderr); the call
	// still succeeds with the readable results.
	out := callGlob(t, s, GlobInput{Pattern: "**"})
	if len(out.Files) != 1 || out.Files[0] != filepath.Join(dir, "ok.txt") {
		t.Errorf("Files = %v, want [ok.txt]; locked dir must be skipped without failing", out.Files)
	}
}

// overrideInt swaps *dst for a test value and returns a restore func; the
// callers defer it. See the file comment about the serial-test contract.
func overrideInt(dst *int, v int) func() {
	old := *dst
	*dst = v
	return func() { *dst = old }
}
