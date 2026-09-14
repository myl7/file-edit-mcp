// read tool tests (ARCHITECTURE §5.1 / §9 matrix rows 2–4, 6).

package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadBasic(t *testing.T) {
	cs, s, dir := startTestServer(t)
	path := filepath.Join(dir, "hello.txt")
	writeDisk(t, path, "l1\nl2\nl3\n", 0o644)

	out := readTool(t, cs, path, nil)
	want := "1\tl1\n2\tl2\n3\tl3"
	if out.Content != want {
		t.Errorf("Content = %q, want %q", out.Content, want)
	}
	if out.Footer != "" {
		t.Errorf("Footer = %q, want empty (read reached EOF)", out.Footer)
	}

	// A successful read records the session marker (§7).
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	m, ok := s.Sess.Lookup(path)
	if !ok {
		t.Fatal("no session marker after read")
	}
	if m.Size != info.Size() {
		t.Errorf("marker size = %d, want %d", m.Size, info.Size())
	}
}

func TestReadOffsetLimitFooter(t *testing.T) {
	cs, _, dir := startTestServer(t)
	path := filepath.Join(dir, "lines.txt")
	writeDisk(t, path, "one\ntwo\nthree\nfour\nfive\n", 0o644)

	// limit=2 from offset=2: lines 2–3, footer reports the range, the
	// binding limit, and the continuation offset (§9 matrix row 2).
	res := readTool(t, cs, path, map[string]any{"offset": 2, "limit": 2})
	if res.Content != "2\ttwo\n3\tthree" {
		t.Errorf("Content = %q", res.Content)
	}
	for _, sub := range []string{"lines 2-3 of 5", "requested limit reached", "continue with offset=4"} {
		if !strings.Contains(res.Footer, sub) {
			t.Errorf("Footer %q missing %q", res.Footer, sub)
		}
	}

	// Continuing at the suggested offset reads the rest, no footer; line
	// numbers stay absolute (§5.1).
	rest := readTool(t, cs, path, map[string]any{"offset": 4})
	if rest.Content != "4\tfour\n5\tfive" {
		t.Errorf("continuation Content = %q", rest.Content)
	}
	if rest.Footer != "" {
		t.Errorf("continuation Footer = %q, want empty", rest.Footer)
	}
}

func TestReadArgsValidation(t *testing.T) {
	cs, _, dir := startTestServer(t)
	path := filepath.Join(dir, "f.txt")
	writeDisk(t, path, "x\n", 0o644)

	wantErr(t, cs, "read", map[string]any{"file_path": path, "offset": -1},
		"offset must be >= 1")
	wantErr(t, cs, "read", map[string]any{"file_path": path, "limit": -3},
		"limit must be >= 1")
}

func TestReadNotExistVersusDir(t *testing.T) {
	cs, _, dir := startTestServer(t)

	// §9 matrix row 4 (ENOENT side): a missing file is ENotExist.
	missing := filepath.Join(dir, "missing.txt")
	wantErr(t, cs, "read", map[string]any{"file_path": missing},
		"file does not exist: "+missing)

	// §5.1: a directory is EIsDir, a different error from ENOENT.
	sub := filepath.Join(dir, "subdir")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	wantErr(t, cs, "read", map[string]any{"file_path": sub},
		"path is a directory, not a file: "+sub)
}

func TestReadInvalidUTF8Sanitized(t *testing.T) {
	cs, _, dir := startTestServer(t)
	path := filepath.Join(dir, "binary.bin")
	writeDisk(t, path, "\xff\xfeok\n", 0o644)

	out := readTool(t, cs, path, nil)
	if !strings.Contains(out.Content, "\uFFFD") {
		t.Errorf("Content %q lacks U+FFFD for invalid bytes", out.Content)
	}
	if !strings.Contains(out.Content, "ok") {
		t.Errorf("Content %q lost the valid tail", out.Content)
	}
}

func TestReadMarksSessionForWrite(t *testing.T) {
	cs, _, dir := startTestServer(t)
	path := filepath.Join(dir, "flow.txt")
	writeDisk(t, path, "before\n", 0o644)

	// The read marker is what unblocks a later write (§5.2).
	readTool(t, cs, path, nil)
	wantOK(t, cs, "write", map[string]any{"file_path": path, "content": "after\n"})
	if got := diskFile(t, path); got != "after\n" {
		t.Errorf("disk = %q, want %q", got, "after\n")
	}
}
