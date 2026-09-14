// write tool tests (ARCHITECTURE §5.2 / §9 matrix rows 5, 8, 9).

package tools

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteNewFileAndMarker(t *testing.T) {
	cs, _, dir := startTestServer(t)
	path := filepath.Join(dir, "new.txt")

	wantOK(t, cs, "write", map[string]any{"file_path": path, "content": "v1\n"})

	if got := diskFile(t, path); got != "v1\n" {
		t.Errorf("disk = %q, want %q", got, "v1\n")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Errorf("new file mode = %o, want 644", perm)
	}
	if residue := tempResidue(t, dir); len(residue) != 0 {
		t.Errorf("temp residue after successful write: %v", residue)
	}

	// §5.2: a successful write marks the content known — a follow-up edit
	// needs no fresh read.
	wantOK(t, cs, "edit", map[string]any{
		"file_path": path, "old_string": "v1", "new_string": "v2",
	})
	if got := diskFile(t, path); got != "v2\n" {
		t.Errorf("disk after edit = %q, want %q", got, "v2\n")
	}
}

func TestWriteUnreadExisting(t *testing.T) {
	cs, _, dir := startTestServer(t)
	path := filepath.Join(dir, "exists.txt")
	writeDisk(t, path, "external\n", 0o644)

	// §9 matrix row 8: exists but never read this session.
	wantErr(t, cs, "write", map[string]any{"file_path": path, "content": "x\n"},
		"file exists but has not been read in this session", path)
	if got := diskFile(t, path); got != "external\n" {
		t.Errorf("disk changed on rejected write: %q", got)
	}
}

func TestWriteOverwriteAfterRead(t *testing.T) {
	cs, _, dir := startTestServer(t)
	path := filepath.Join(dir, "rw.txt")
	writeDisk(t, path, "old\n", 0o644)

	readTool(t, cs, path, nil)
	wantOK(t, cs, "write", map[string]any{"file_path": path, "content": "new content\n"})
	if got := diskFile(t, path); got != "new content\n" {
		t.Errorf("disk = %q", got)
	}
}

func TestWritePreservesMode(t *testing.T) {
	cs, _, dir := startTestServer(t)
	path := filepath.Join(dir, "mode.txt")
	writeDisk(t, path, "x\n", 0o600)

	readTool(t, cs, path, nil)
	wantOK(t, cs, "write", map[string]any{"file_path": path, "content": "y\n"})
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600 (§5.2: existing mode carries over)", perm)
	}
}

func TestWriteParentMissing(t *testing.T) {
	cs, _, dir := startTestServer(t)
	path := filepath.Join(dir, "no", "such", "dir", "f.txt")

	// §9 matrix row 5: no implicit directory creation.
	wantErr(t, cs, "write", map[string]any{"file_path": path, "content": "x\n"},
		"parent directory does not exist")
}

func TestWriteIsDir(t *testing.T) {
	cs, _, dir := startTestServer(t)
	wantErr(t, cs, "write", map[string]any{"file_path": dir, "content": "x\n"},
		"path is a directory, not a file")
}

func TestWriteSubdirectory(t *testing.T) {
	cs, _, dir := startTestServer(t)
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(sub, "nested.txt")
	wantOK(t, cs, "write", map[string]any{"file_path": path, "content": "deep\n"})
	if got := diskFile(t, path); got != "deep\n" {
		t.Errorf("disk = %q", got)
	}
	if residue := tempResidue(t, dir); len(residue) != 0 {
		t.Errorf("temp residue in subdirectory write: %v", residue)
	}
}

// TestWriteAtomicExistingFile: an injected rename failure (§9 row 9 — a
// read-only target directory is not a portable way to force this, so the
// rename step is a field on Server) must leave the target untouched and no
// .file-edit-mcp-* temp file behind.
func TestWriteAtomicExistingFile(t *testing.T) {
	cs, s, dir := startTestServer(t)
	path := filepath.Join(dir, "target.txt")
	writeDisk(t, path, "original\n", 0o644)
	readTool(t, cs, path, nil)

	s.rename = func(_ *os.Root, _, _ string) error { return errors.New("injected rename failure") }
	wantErr(t, cs, "write", map[string]any{"file_path": path, "content": "replacement\n"},
		"injected rename failure")

	if got := diskFile(t, path); got != "original\n" {
		t.Errorf("target content = %q, want the original (atomicity)", got)
	}
	if residue := tempResidue(t, dir); len(residue) != 0 {
		t.Errorf("temp residue after failed write: %v", residue)
	}
}

// TestWriteAtomicNewFile: same injection on a new file — the O_EXCL claim
// and the temp file are both cleaned up, restoring the pre-call state.
func TestWriteAtomicNewFile(t *testing.T) {
	cs, s, dir := startTestServer(t)
	path := filepath.Join(dir, "claimed.txt")

	s.rename = func(_ *os.Root, _, _ string) error { return errors.New("injected rename failure") }
	wantErr(t, cs, "write", map[string]any{"file_path": path, "content": "x\n"},
		"injected rename failure")

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("claimed file left behind after failed write (stat err = %v)", err)
	}
	if residue := tempResidue(t, dir); len(residue) != 0 {
		t.Errorf("temp residue after failed new-file write: %v", residue)
	}
}

// TestWriteRecoverAfterInjection: the injection is per-Server and does not
// leak into later calls — sanity check with a fresh successful write.
func TestWriteRecoverAfterInjection(t *testing.T) {
	cs, s, dir := startTestServer(t)
	path := filepath.Join(dir, "recover.txt")
	writeDisk(t, path, "a\n", 0o644)
	readTool(t, cs, path, nil)

	s.rename = func(_ *os.Root, _, _ string) error { return errors.New("injected rename failure") }
	wantErr(t, cs, "write", map[string]any{"file_path": path, "content": "b\n"},
		"injected rename failure")

	s.rename = func(r *os.Root, oldname, newname string) error {
		return r.Rename(oldname, newname)
	}
	wantOK(t, cs, "write", map[string]any{"file_path": path, "content": "c\n"})
	if got := diskFile(t, path); got != "c\n" {
		t.Errorf("disk after recovery = %q", got)
	}
}
