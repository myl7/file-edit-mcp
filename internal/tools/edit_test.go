// edit and multi_edit tool tests (ARCHITECTURE §5.3/§5.4 / §9 matrix rows
// 1, 4, 7, 8).

package tools

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestEditBasicAndMarkerRefresh(t *testing.T) {
	cs, _, dir := startTestServer(t)
	path := filepath.Join(dir, "edit.txt")
	writeDisk(t, path, "alpha\nbeta\n", 0o644)

	readTool(t, cs, path, nil)
	wantOK(t, cs, "edit", map[string]any{
		"file_path": path, "old_string": "beta", "new_string": "gamma",
	})
	if got := diskFile(t, path); got != "alpha\ngamma\n" {
		t.Errorf("disk = %q, want %q", got, "alpha\ngamma\n")
	}
	if residue := tempResidue(t, dir); len(residue) != 0 {
		t.Errorf("temp residue after edit: %v", residue)
	}

	// The write refreshes the marker: a second edit needs no re-read.
	wantOK(t, cs, "edit", map[string]any{
		"file_path": path, "old_string": "gamma", "new_string": "delta",
	})
	if got := diskFile(t, path); got != "alpha\ndelta\n" {
		t.Errorf("disk after second edit = %q", got)
	}
}

// TestEditNotExistDistinctFromNoMatch: §9 matrix row 4 — a missing file and
// a present file without the anchor are two different errors.
func TestEditNotExistDistinctFromNoMatch(t *testing.T) {
	cs, _, dir := startTestServer(t)
	missing := filepath.Join(dir, "missing.txt")
	present := filepath.Join(dir, "present.txt")
	writeDisk(t, present, "content\n", 0o644)
	readTool(t, cs, present, nil)

	missingMsg := wantErr(t, cs, "edit", map[string]any{
		"file_path": missing, "old_string": "x", "new_string": "y",
	}, "file does not exist: "+missing)
	nomatchMsg := wantErr(t, cs, "edit", map[string]any{
		"file_path": present, "old_string": "absent-anchor", "new_string": "y",
	}, "old_string not found")

	if missingMsg == nomatchMsg {
		t.Errorf("ENotExist and ENoMatch render identically: %q", missingMsg)
	}
	if !strings.Contains(missingMsg, "file does not exist") {
		t.Errorf("ENotExist message %q lacks its catalog text", missingMsg)
	}
	if !strings.Contains(nomatchMsg, present) {
		t.Errorf("ENoMatch message %q lacks the path context", nomatchMsg)
	}
}

func TestEditUnread(t *testing.T) {
	cs, _, dir := startTestServer(t)
	path := filepath.Join(dir, "unread.txt")
	writeDisk(t, path, "data\n", 0o644)

	wantErr(t, cs, "edit", map[string]any{
		"file_path": path, "old_string": "data", "new_string": "x",
	}, "file exists but has not been read in this session", path)
}

func TestEditStaleRead(t *testing.T) {
	cs, _, dir := startTestServer(t)
	path := filepath.Join(dir, "stale.txt")
	writeDisk(t, path, "short\n", 0o644)

	readTool(t, cs, path, nil)

	// §9 matrix row 8: the file changes after the read (different size, and
	// an explicit mtime bump so coarse-grained clocks cannot mask it).
	writeDisk(t, path, "much longer now\n", 0o644)
	if err := os.Chtimes(path, time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}

	wantErr(t, cs, "edit", map[string]any{
		"file_path": path, "old_string": "short", "new_string": "x",
	}, "file changed since last read", path)
}

// TestEditMidFlightChangeStale: the pre-write stat guard (§5.3) — a change
// landing between the handler's read and its write is rejected, not
// silently overwritten. The apply callback runs exactly in that window, so
// driving editFlow directly lets the test change the file mid-flight.
func TestEditMidFlightChangeStale(t *testing.T) {
	_, s, dir := startTestServer(t)
	path := filepath.Join(dir, "midflight.txt")
	writeDisk(t, path, "v1\n", 0o644)

	// Seed the marker without the MCP client: a direct read via the handler
	// internals is overkill; MarkRead mirrors what Read does.
	if info, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else {
		s.Sess.MarkRead(path, info.Size(), info.ModTime())
	}

	err := s.editFlow(path, func(content []byte) ([]byte, error) {
		// Concurrent external writer lands mid-edit.
		writeDisk(t, path, "external rewrite\n", 0o644)
		return append(content, 'X'), nil
	})
	if err == nil || !strings.Contains(err.Error(), "file changed since last read") {
		t.Fatalf("editFlow err = %v, want EStaleRead", err)
	}
	if got := diskFile(t, path); got != "external rewrite\n" {
		t.Errorf("disk = %q, want the external writer's content", got)
	}
}

func TestEditAmbiguous(t *testing.T) {
	cs, _, dir := startTestServer(t)
	path := filepath.Join(dir, "dup.txt")
	writeDisk(t, path, "dup\ndup\ndup\n", 0o644)
	readTool(t, cs, path, nil)

	// §9 matrix row 1: match count plus first line numbers.
	wantErr(t, cs, "edit", map[string]any{
		"file_path": path, "old_string": "dup", "new_string": "x",
	}, "matches 3 times", "line(s) 1, 2, 3")
	if got := diskFile(t, path); got != "dup\ndup\ndup\n" {
		t.Errorf("disk changed on ambiguous edit: %q", got)
	}
}

func TestEditNoMatchByteExact(t *testing.T) {
	cs, _, dir := startTestServer(t)
	path := filepath.Join(dir, "indent.txt")
	writeDisk(t, path, "\tindented\n", 0o644) // tab indent
	readTool(t, cs, path, nil)

	// §5.3: no trim, no whitespace normalization — a visually identical
	// anchor using spaces instead of the tab does not match.
	wantErr(t, cs, "edit", map[string]any{
		"file_path": path, "old_string": "    indented", "new_string": "x",
	}, "old_string not found", path)
}

func TestEditReplaceAll(t *testing.T) {
	cs, _, dir := startTestServer(t)
	path := filepath.Join(dir, "many.txt")
	writeDisk(t, path, "v\nv\nv\n", 0o644)
	readTool(t, cs, path, nil)

	wantOK(t, cs, "edit", map[string]any{
		"file_path": path, "old_string": "v", "new_string": "w", "replace_all": true,
	})
	if got := diskFile(t, path); got != "w\nw\nw\n" {
		t.Errorf("disk = %q, want %q", got, "w\nw\nw\n")
	}
}

func TestEditNoopAndEmptyOld(t *testing.T) {
	cs, _, dir := startTestServer(t)
	path := filepath.Join(dir, "noop.txt")
	writeDisk(t, path, "same\n", 0o644)
	readTool(t, cs, path, nil)

	wantErr(t, cs, "edit", map[string]any{
		"file_path": path, "old_string": "same", "new_string": "same",
	}, "old_string is identical to new_string")

	wantErr(t, cs, "edit", map[string]any{
		"file_path": path, "old_string": "", "new_string": "x",
	}, "old_string is empty")
}

// --- multi_edit ------------------------------------------------------------

func TestMultiEditSequentialAnchors(t *testing.T) {
	cs, _, dir := startTestServer(t)
	path := filepath.Join(dir, "multi.txt")
	writeDisk(t, path, "a\nb\n", 0o644)
	readTool(t, cs, path, nil)

	// The second edit anchors on text only the first edit produced (§5.4).
	wantOK(t, cs, "multi_edit", map[string]any{
		"file_path": path,
		"edits": []map[string]any{
			{"old_string": "a", "new_string": "A"},
			{"old_string": "A\nb", "new_string": "X"},
		},
	})
	if got := diskFile(t, path); got != "X\n" {
		t.Errorf("disk = %q, want %q", got, "X\n")
	}
}

func TestMultiEditFailureIsAtomic(t *testing.T) {
	cs, _, dir := startTestServer(t)
	path := filepath.Join(dir, "atomic.txt")
	writeDisk(t, path, "keep\n", 0o644)
	readTool(t, cs, path, nil)

	// §9 matrix row 7: edit #2 fails → nothing lands, content unchanged.
	wantErr(t, cs, "multi_edit", map[string]any{
		"file_path": path,
		"edits": []map[string]any{
			{"old_string": "keep", "new_string": "KEEP"},
			{"old_string": "no-such-anchor", "new_string": "x"},
		},
	}, "edit #2 failed", "old_string not found")

	if got := diskFile(t, path); got != "keep\n" {
		t.Errorf("disk = %q, want %q (no partial application)", got, "keep\n")
	}
	if residue := tempResidue(t, dir); len(residue) != 0 {
		t.Errorf("temp residue after failed multi_edit: %v", residue)
	}
}

func TestMultiEditUnreadAndEmpty(t *testing.T) {
	cs, _, dir := startTestServer(t)
	path := filepath.Join(dir, "mu.txt")
	writeDisk(t, path, "x\n", 0o644)

	wantErr(t, cs, "multi_edit", map[string]any{
		"file_path": path,
		"edits":     []map[string]any{{"old_string": "x", "new_string": "y"}},
	}, "has not been read in this session")

	readTool(t, cs, path, nil)
	wantErr(t, cs, "multi_edit", map[string]any{
		"file_path": path, "edits": []map[string]any{},
	}, "at least one edit")
}

// --- concurrency -----------------------------------------------------------

// TestConcurrentEditsSameFile: two concurrent edits on one file are
// serialized by the per-path session lock (§7): both succeed (each handler
// re-reads under the lock and matches the refreshed marker) and both
// replacements land. Run under -race.
func TestConcurrentEditsSameFile(t *testing.T) {
	cs, _, dir := startTestServer(t)
	path := filepath.Join(dir, "race.txt")
	writeDisk(t, path, "l1\nl2\nl3\nl4\n", 0o644)
	readTool(t, cs, path, nil)

	anchors := []struct{ old, new string }{
		{"l1", "E1"},
		{"l3", "E3"},
	}
	var wg sync.WaitGroup
	errs := make([]string, len(anchors))
	for i, a := range anchors {
		wg.Add(1)
		go func(i int, old, new string) {
			defer wg.Done()
			text, isErr := callTool(t, cs, "edit", map[string]any{
				"file_path": path, "old_string": old, "new_string": new,
			})
			if isErr {
				errs[i] = text
			}
		}(i, a.old, a.new)
	}
	wg.Wait()

	for i, msg := range errs {
		if msg != "" {
			t.Errorf("concurrent edit %d failed: %s", i, msg)
		}
	}
	final := diskFile(t, path)
	for _, want := range []string{"E1", "E3"} {
		if !strings.Contains(final, want) {
			t.Errorf("final content %q missing %q (lost update)", final, want)
		}
	}
	if residue := tempResidue(t, dir); len(residue) != 0 {
		t.Errorf("temp residue after concurrent edits: %v", residue)
	}
}
