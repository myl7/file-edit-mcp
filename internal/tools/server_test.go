// Shared test harness: a real Server over a t.TempDir allowed directory,
// driven through an in-memory MCP client session (the §9 integration-layer
// pattern), plus path-escape matrix tests that exercise the pathguard
// pipeline end-to-end through the read tool.

package tools

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// startTestServer boots a Server over one fresh temp allowed dir and returns
// the connected client session, the Conn (for direct inspection/injection
// in same-package tests), and the canonicalized allowed dir to build paths
// with (t.TempDir may contain symlinked components on macOS; the guard
// canonicalizes them, so assertions must use the resolved form).
func startTestServer(t *testing.T) (*mcp.ClientSession, *Conn, string) {
	t.Helper()
	return startTestServerAt(t, t.TempDir())
}

func startTestServerAt(t *testing.T, dir string) (*mcp.ClientSession, *Conn, string) {
	t.Helper()
	sh, err := NewShared([]string{dir}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewShared: %v", err)
	}
	s := sh.NewConn()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", dir, err)
	}

	srv := mcp.NewServer(&mcp.Implementation{Name: "file-edit-mcp", Version: "test"}, nil)
	s.Register(srv)

	ct, st := mcp.NewInMemoryTransports()
	ctx := context.Background()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() {
		_ = cs.Close()
		_ = ss.Close()
	})
	return cs, s, real
}

// callTool invokes a tool and returns its first text block plus IsError.
// It reports transport failures with Errorf (not Fatalf) so it is safe to
// call from test goroutines; callers treat ("" , true) as a failed call.
func callTool(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Errorf("%s: transport/protocol error: %v", name, err)
		return "", true
	}
	text := ""
	if len(res.Content) > 0 {
		if tc, ok := res.Content[0].(*mcp.TextContent); ok {
			text = tc.Text
		}
	}
	return text, res.IsError
}

// wantOK asserts a successful call and returns its text.
func wantOK(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) string {
	t.Helper()
	text, isErr := callTool(t, cs, name, args)
	if isErr {
		t.Fatalf("%s: unexpected tool error: %s", name, text)
	}
	return text
}

// wantErr asserts a failed call whose text contains every substring.
func wantErr(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any, substrs ...string) string {
	t.Helper()
	text, isErr := callTool(t, cs, name, args)
	if !isErr {
		t.Fatalf("%s: expected tool error, got success: %s", name, text)
	}
	for _, sub := range substrs {
		if !strings.Contains(text, sub) {
			t.Errorf("%s: error text %q missing substring %q", name, text, sub)
		}
	}
	return text
}

// diskFile reads a file straight from disk (the ground truth next to the
// MCP-visible results).
func diskFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// writeDisk creates/overwrites a file on disk outside the MCP layer.
func writeDisk(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// tempResidue lists leftover atomic-write temp files anywhere under dir
// (§9.9: a failed or successful write must leave none).
func tempResidue(t *testing.T, dir string) []string {
	t.Helper()
	var found []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasPrefix(d.Name(), tempPrefix) {
			found = append(found, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return found
}

// readTool invokes read and decodes the structured ReadOutput; extraArgs
// merges over the file_path argument (nil for none).
func readTool(t *testing.T, cs *mcp.ClientSession, path string, extraArgs map[string]any) ReadOutput {
	t.Helper()
	args := map[string]any{"file_path": path}
	for k, v := range extraArgs {
		args[k] = v
	}
	text := wantOK(t, cs, "read", args)
	var out ReadOutput
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("read result is not ReadOutput JSON: %v (%s)", err, text)
	}
	return out
}

// TestRegisterListsAllTools verifies a client sees exactly the six tools
// with non-empty descriptions after initialize + tools/list.
func TestRegisterListsAllTools(t *testing.T) {
	cs, _, _ := startTestServer(t)

	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"read", "write", "edit", "multi_edit", "glob", "grep"}
	if len(res.Tools) != len(want) {
		t.Fatalf("got %d tools, want %d", len(res.Tools), len(want))
	}
	got := make(map[string]bool, len(res.Tools))
	for _, tool := range res.Tools {
		got[tool.Name] = true
		if tool.Description == "" {
			t.Errorf("tool %q has empty description", tool.Name)
		}
	}
	for _, name := range want {
		if !got[name] {
			t.Errorf("tool %q missing from tools/list", name)
		}
	}
}

// TestServerRejectsBadAllowedDirs: startup validation via NewShared.
func TestServerRejectsBadAllowedDirs(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "gone")
	if _, err := NewShared([]string{missing}, nil); err == nil {
		t.Error("NewShared accepted a missing allowed dir, want error")
	}
	if _, err := NewShared(nil, nil); err == nil {
		t.Error("NewShared accepted no allowed dirs, want error")
	}
}

// TestPathEscapeMatrix drives the §3 pipeline end-to-end through the read
// tool (all four tools share Guard.Validate, so one tool covers the
// matrix): relative, outside absolute, .. traversal, Windows drive, and a
// real symlink pointing out of the allowed tree.
func TestPathEscapeMatrix(t *testing.T) {
	cs, _, dir := startTestServer(t)
	writeDisk(t, filepath.Join(dir, "f.txt"), "x\n", 0o644)

	t.Run("relative", func(t *testing.T) {
		wantErr(t, cs, "read", map[string]any{"file_path": "f.txt"},
			"path is not absolute")
	})

	t.Run("outside absolute", func(t *testing.T) {
		wantErr(t, cs, "read", map[string]any{"file_path": "/etc/hosts"},
			"outside the allowed directories")
	})

	t.Run("dotdot traversal", func(t *testing.T) {
		escape := filepath.Join(dir, "sub", "..", "..", "escape.txt")
		wantErr(t, cs, "read", map[string]any{"file_path": escape},
			"outside the allowed directories")
	})

	t.Run("windows drive", func(t *testing.T) {
		wantErr(t, cs, "read", map[string]any{"file_path": "C:\\x"},
			"looks like a Windows path")
	})

	t.Run("symlink out", func(t *testing.T) {
		outside := filepath.Join(filepath.Dir(dir), "outside-target-"+filepath.Base(dir))
		writeDisk(t, outside, "secret\n", 0o644)
		link := filepath.Join(dir, "link-out")
		if err := os.Symlink(outside, link); err != nil {
			t.Skipf("symlink: %v", err)
		}
		wantErr(t, cs, "read", map[string]any{"file_path": link},
			"outside the allowed directories")
	})
}

// TestConcurrentMixedTools: parallel reads/writes/edits on distinct paths
// through one server session (race-detector fodder for the shared Session,
// RootPool, and Retrier).
func TestConcurrentMixedTools(t *testing.T) {
	cs, _, dir := startTestServer(t)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := filepath.Join(dir, "f"+string(rune('a'+i))+".txt")
			path := map[string]any{"file_path": name}
			if _, isErr := callTool(t, cs, "write", map[string]any{"file_path": name, "content": "v0\n"}); isErr {
				t.Errorf("write %s failed", name)
				return
			}
			if _, isErr := callTool(t, cs, "read", path); isErr {
				t.Errorf("read %s failed", name)
				return
			}
			if _, isErr := callTool(t, cs, "edit", map[string]any{
				"file_path": name, "old_string": "v0", "new_string": "v1",
			}); isErr {
				t.Errorf("edit %s failed", name)
			}
		}(i)
	}
	wg.Wait()
}
