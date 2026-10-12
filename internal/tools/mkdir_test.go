// mkdir tool tests (§5.8/§9): single-level creation, idempotency, the
// existing-target verdicts, and the pathguard boundary cases — driven
// through the full in-memory server like the other tools.

package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// mcpClientSession aliases the SDK session type so test signatures stay short.
type mcpClientSession = mcp.ClientSession

// mkdirTool invokes mkdir and returns its decoded output + IsError.
func mkdirTool(t *testing.T, cs *mcpClientSession, dirPath string) (MkdirOutput, bool) {
	t.Helper()
	text, isErr := callTool(t, cs, "mkdir", map[string]any{"dir_path": dirPath})
	var out MkdirOutput
	if isErr {
		return out, true
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("mkdir result is not MkdirOutput JSON: %v (%s)", err, text)
	}
	return out, false
}

func TestMkdirLifecycle(t *testing.T) {
	cs, _, dir := startTestServer(t)

	out, isErr := mkdirTool(t, cs, filepath.Join(dir, "attachments"))
	if isErr {
		t.Fatal("mkdir failed")
	}
	if out.Status != "created" {
		t.Errorf("first mkdir status = %q, want created", out.Status)
	}
	if info, err := os.Stat(filepath.Join(dir, "attachments")); err != nil || !info.IsDir() {
		t.Fatalf("directory not on disk: %v", err)
	}

	// Idempotent: the same path reports exists.
	out, isErr = mkdirTool(t, cs, filepath.Join(dir, "attachments"))
	if isErr || out.Status != "exists" {
		t.Errorf("second mkdir = (%q, isErr=%v), want exists", out.Status, isErr)
	}

	// The allowed root itself already exists.
	out, isErr = mkdirTool(t, cs, dir)
	if isErr || out.Status != "exists" {
		t.Errorf("mkdir on the root = (%q, isErr=%v), want exists", out.Status, isErr)
	}
}

func TestMkdirRejections(t *testing.T) {
	cs, _, dir := startTestServer(t)
	writeDisk(t, filepath.Join(dir, "file.txt"), "x", 0o644)

	// Existing regular file: ENotDir.
	_, isErr := mkdirTool(t, cs, filepath.Join(dir, "file.txt"))
	if !isErr {
		t.Fatal("mkdir over a regular file succeeded")
	}

	// Single level only: a missing parent is EParentMissing, never an
	// implicit MkdirAll.
	if _, isErr := mkdirTool(t, cs, filepath.Join(dir, "missing", "deeper")); !isErr {
		t.Fatal("multi-level mkdir without parents succeeded")
	}
	if _, err := os.Stat(filepath.Join(dir, "missing")); !os.IsNotExist(err) {
		t.Error("mkdir created intermediate directories")
	}

	// Path escapes still apply.
	_, isErr = mkdirTool(t, cs, "/etc/definitely-not-allowed")
	if !isErr {
		t.Fatal("mkdir outside the allowed roots succeeded")
	}

	// Relative paths are rejected by the shared pipeline.
	_, isErr = mkdirTool(t, cs, "relative-dir")
	if !isErr {
		t.Fatal("relative mkdir succeeded")
	}
}

// TestMkdirThenWriteInside: the documented agent flow — mkdir one level,
// then work inside it — proving no read marker is needed for mkdir (only
// files carry markers, never directories).
func TestMkdirThenWriteInside(t *testing.T) {
	cs, _, dir := startTestServer(t)
	if _, isErr := mkdirTool(t, cs, filepath.Join(dir, "notes")); isErr {
		t.Fatal("mkdir failed")
	}
	text, isErr := callTool(t, cs, "write", map[string]any{
		"file_path": filepath.Join(dir, "notes", "a.txt"), "content": "hi\n"})
	if isErr {
		t.Fatalf("write inside fresh dir failed: %s", text)
	}
}
