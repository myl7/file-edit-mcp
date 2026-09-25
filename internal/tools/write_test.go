// write tool tests (ARCHITECTURE §5.2 / §9 matrix rows 5, 8, 9).

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// writeAckJSON mirrors writeAck for decoding a success text block (the ack
// JSON): substring-matching the raw text cannot work for Warning, whose
// payload contains characters JSON escapes.
type writeAckJSON struct {
	BytesWritten int64  `json:"bytes_written"`
	Warning      string `json:"warning"`
}

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

	// §9 matrix row 8: exists but never read this session — the never-read
	// EStaleRead variant (the gate's stale wording must NOT fire here).
	wantErr(t, cs, "write", map[string]any{"file_path": path, "content": "x\n"},
		"file has not been read in this session", path)
	if got := diskFile(t, path); got != "external\n" {
		t.Errorf("disk changed on rejected write: %q", got)
	}
}

// TestWriteStaleAfterOutOfBandChange pins the unified gate on write: a
// wholesale rewrite of a file that changed since the caller's read is
// EStaleRead — this exact call silently succeeded before write adopted
// edit's fence — and the remedy is the same as edit's: re-read, then write.
func TestWriteStaleAfterOutOfBandChange(t *testing.T) {
	cs, _, dir := startTestServer(t)
	path := filepath.Join(dir, "stale-write.txt")
	writeDisk(t, path, "v1\n", 0o644)

	readTool(t, cs, path, nil)

	// §9 matrix row 8 shape, now for write too: the file changes after the
	// read (different size, and an explicit mtime bump so coarse-grained
	// clocks cannot mask it).
	writeDisk(t, path, "external editor\n", 0o644)
	if err := os.Chtimes(path, time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}

	wantErr(t, cs, "write", map[string]any{"file_path": path, "content": "mine\n"},
		"file changed since last read", path)
	if got := diskFile(t, path); got != "external editor\n" {
		t.Errorf("disk changed on rejected write: %q", got)
	}

	// Re-read re-arms the gate: the same wholesale write now goes through.
	readTool(t, cs, path, nil)
	wantOK(t, cs, "write", map[string]any{"file_path": path, "content": "mine\n"})
	if got := diskFile(t, path); got != "mine\n" {
		t.Errorf("disk after re-read + write = %q, want %q", got, "mine\n")
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

// TestWriteContentWarningCorrupted (§5.7): a write whose content carries
// real U+0008/U+000B (the upstream-escape-corruption signature) succeeds
// unchanged, but the success ack carries the warning — and the same warning
// is mirrored into structuredContent. The disk bytes are exactly what was
// sent; the write itself is never affected.
func TestWriteContentWarningCorrupted(t *testing.T) {
	cs, _, dir := startTestServer(t)
	path := filepath.Join(dir, "corrupted.txt")
	// Line 2: the 12-rune prefix "R_i - R_f = " puts the U+0008 at col 13
	// and the U+000B after "eta_i (R_m - R_f) + " at col 34.
	content := "line one\nR_i - R_f = \x08eta_i (R_m - R_f) + \x0bepsilon_i\n"

	text := wantOK(t, cs, "write", map[string]any{"file_path": path, "content": content})
	var ack writeAckJSON
	if err := json.Unmarshal([]byte(text), &ack); err != nil {
		t.Fatalf("write result is not writeAck JSON: %v (%s)", err, text)
	}
	if !strings.HasPrefix(ack.Warning, "content warning: content contains") {
		t.Errorf("Warning = %q, want prefix %q", ack.Warning, "content warning: content contains")
	}
	if !strings.Contains(ack.Warning, "U+0008 (BACKSPACE) at line 2 col 13") {
		t.Errorf("Warning %q missing the U+0008 position", ack.Warning)
	}
	if !strings.Contains(ack.Warning, "U+000B (LINE TABULATION)") {
		t.Errorf("Warning %q missing the U+000B group", ack.Warning)
	}
	if !strings.Contains(ack.Warning, "U+000B (LINE TABULATION) at line 2 col 34") {
		t.Errorf("Warning %q missing the U+000B position", ack.Warning)
	}
	if ack.BytesWritten != int64(len(content)) {
		t.Errorf("BytesWritten = %d, want %d", ack.BytesWritten, len(content))
	}
	if got := diskFile(t, path); got != content {
		t.Errorf("disk = %q, want the sent bytes verbatim %q (§5.7: never rewritten)", got, content)
	}

	// The SDK mirrors the ack into structuredContent: the warning must ride
	// there too (driven with cs.CallTool directly, as callTool does).
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "write",
		Arguments: map[string]any{"file_path": path, "content": content},
	})
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected tool error on re-write: %v", res.Content)
	}
	sc, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("StructuredContent is %T, want map[string]any", res.StructuredContent)
	}
	if got, want := sc["warning"], ack.Warning; got != want {
		t.Errorf("structuredContent warning = %v, want %q", got, want)
	}
	if residue := tempResidue(t, dir); len(residue) != 0 {
		t.Errorf("temp residue after warned write: %v", residue)
	}
}

// TestWriteCleanLatexNoWarning (§5.7): literal backslash escapes — the text
// the caller meant to send — produce no warning at all.
func TestWriteCleanLatexNoWarning(t *testing.T) {
	cs, _, dir := startTestServer(t)
	path := filepath.Join(dir, "clean.tex")
	content := "$\n\\beta_i + \\varepsilon_i\n$$" // literal backslashes at runtime

	text := wantOK(t, cs, "write", map[string]any{"file_path": path, "content": content})
	if strings.Contains(text, "warning") {
		t.Errorf("clean write text mentions warning: %s", text)
	}
	var ack writeAckJSON
	if err := json.Unmarshal([]byte(text), &ack); err != nil {
		t.Fatalf("write result is not writeAck JSON: %v (%s)", err, text)
	}
	if ack.Warning != "" {
		t.Errorf("Warning = %q, want empty", ack.Warning)
	}
}

// TestWriteNormalTextNoWarning (§5.7): tabs, newlines, carriage returns and
// ordinary bracket/underscore notation are never suspicious — absence is
// asserted on the raw ack text, where the omitempty field is exact.
func TestWriteNormalTextNoWarning(t *testing.T) {
	cs, _, dir := startTestServer(t)

	tabRich := filepath.Join(dir, "tabs.txt")
	text := wantOK(t, cs, "write", map[string]any{
		"file_path": tabRich, "content": "a\tb\nc\td\r\ne\n",
	})
	if strings.Contains(text, "warning") {
		t.Errorf("tab/newline-rich write reported a warning: %s", text)
	}

	// Existing target through the freshness fence: same absence.
	bracket := filepath.Join(dir, "bracket.txt")
	writeDisk(t, bracket, "old\n", 0o644)
	readTool(t, cs, bracket, nil)
	text = wantOK(t, cs, "write", map[string]any{
		"file_path": bracket, "content": "alpha_i = [x]\n",
	})
	if strings.Contains(text, "warning") {
		t.Errorf("plain [x]/alpha_i write reported a warning: %s", text)
	}
	if got := diskFile(t, bracket); got != "alpha_i = [x]\n" {
		t.Errorf("disk = %q", got)
	}
}
