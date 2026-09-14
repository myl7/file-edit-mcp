// HTTP transport tests (ARCHITECTURE §0/§9): the real StreamableHTTP wiring
// from main.go served by httptest and driven by the SDK's StreamableHTTP
// client — one write→read E2E round trip, the per-connection session
// isolation (a read on connection A must not unlock an edit on connection
// B; a cross-connection concurrent write must surface as EStaleRead on the
// connection whose marker went stale), the token-path routing (everything
// but /{token}/mcp is a mux 404), and the initialize-result instructions.

package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/myl7/file-edit-mcp/internal/tools"
)

// testToken is the fixed token the HTTP tests configure via tokenEnv — the
// same loadToken+newMCPHandler path run() takes, never a hand-built mux.
const testToken = "test-token-1"

// startHTTPTestServer boots the production HTTP assembly (loadToken +
// newMCPHandler) over one fresh temp allowed dir, exactly as run() and
// runHTTP do minus the http.Server/keepalive plumbing.
func startHTTPTestServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", dir, err)
	}
	t.Setenv(tokenEnv, testToken)
	token, err := loadToken()
	if err != nil {
		t.Fatalf("loadToken: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	shared, err := tools.NewShared([]string{dir}, logger)
	if err != nil {
		t.Fatalf("tools.NewShared: %v", err)
	}
	ts := httptest.NewServer(newMCPHandler(shared, logger, token))
	t.Cleanup(ts.Close)
	return ts, real
}

// connectHTTP opens one MCP client session against ts — one call = one
// StreamableHTTP session = one tools.Conn, the §0 isolation unit. The
// endpoint carries the token in the path, as production clients must.
func connectHTTP(t *testing.T, ts *httptest.Server) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "http-test-client", Version: "1.0"}, nil)
	cs, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: ts.URL + "/" + testToken + "/mcp",
	}, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// httpCallTool invokes a tool and returns its first text block plus IsError.
func httpCallTool(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
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

// TestHTTPWriteReadE2E: one session, write a new file then read it back —
// the write→read round trip over the full HTTP stack.
func TestHTTPWriteReadE2E(t *testing.T) {
	ts, dir := startHTTPTestServer(t)
	cs := connectHTTP(t, ts)
	path := filepath.Join(dir, "e2e.txt")

	text, isErr := httpCallTool(t, cs, "write", map[string]any{
		"file_path": path, "content": "hello\nhttp\n",
	})
	if isErr {
		t.Fatalf("write: unexpected tool error: %s", text)
	}
	if !strings.Contains(text, `"bytes_written":11`) {
		t.Errorf("write ack = %q, want bytes_written 11", text)
	}

	readCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := cs.CallTool(readCtx, &mcp.CallToolParams{
		Name:      "read",
		Arguments: map[string]any{"file_path": path},
	})
	if err != nil {
		t.Fatalf("read: transport error: %v", err)
	}
	if res.IsError {
		t.Fatal("read: unexpected tool error")
	}
	var out struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(res.Content[0].(*mcp.TextContent).Text), &out); err != nil {
		t.Fatalf("read result is not JSON: %v", err)
	}
	if out.Content != "1\thello\n2\thttp" {
		t.Errorf("read Content = %q, want the two written lines", out.Content)
	}

	// The write reached the real disk through the Root pool.
	if b, err := os.ReadFile(path); err != nil || string(b) != "hello\nhttp\n" {
		t.Errorf("disk = %q (%v), want the written bytes", b, err)
	}
}

// TestHTTPSessionIsolation: connection A's read does not unlock connection
// B's edit (per-connection session markers); after B reads and edits, A's
// stale marker rejects A's next edit as EStaleRead — the cross-connection
// safety net for the deliberately unserialized same-file writes (§7).
func TestHTTPSessionIsolation(t *testing.T) {
	ts, dir := startHTTPTestServer(t)
	a := connectHTTP(t, ts)
	b := connectHTTP(t, ts)
	path := filepath.Join(dir, "shared.txt")
	if err := os.WriteFile(path, []byte("v0\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A reads; the marker lands in A's session only.
	if text, isErr := httpCallTool(t, a, "read", map[string]any{"file_path": path}); isErr {
		t.Fatalf("A read: unexpected error: %s", text)
	}

	// B edits without having read: EUnreadWrite — A's read must not leak.
	text, isErr := httpCallTool(t, b, "edit", map[string]any{
		"file_path": path, "old_string": "v0", "new_string": "from B",
	})
	if !isErr || !strings.Contains(text, "has not been read in this session") {
		t.Fatalf("B edit before B read: (isErr=%v) %q, want EUnreadWrite", isErr, text)
	}

	// B reads, then B's edit succeeds on B's own marker.
	if text, isErr := httpCallTool(t, b, "read", map[string]any{"file_path": path}); isErr {
		t.Fatalf("B read: unexpected error: %s", text)
	}
	if text, isErr := httpCallTool(t, b, "edit", map[string]any{
		"file_path": path, "old_string": "v0", "new_string": "from B",
	}); isErr {
		t.Fatalf("B edit after B read: unexpected error: %s", text)
	}

	// A's marker is now stale (B's write moved size/mtime): A's edit is
	// rejected as EStaleRead instead of silently overwriting B's content.
	text, isErr = httpCallTool(t, a, "edit", map[string]any{
		"file_path": path, "old_string": "from B", "new_string": "from A",
	})
	if !isErr || !strings.Contains(text, "file changed since last read") {
		t.Fatalf("A edit after B's write: (isErr=%v) %q, want EStaleRead", isErr, text)
	}
	if got, _ := os.ReadFile(path); string(got) != "from B\n" {
		t.Errorf("disk = %q, want B's content untouched by A's rejected edit", got)
	}
}

// initRequestBody is an initialize-shaped JSON-RPC POST for the routing
// probes below: the mux rejects wrong paths before the SDK handler parses
// anything, but the body is kept realistic so the test also fails loudly if
// routing ever lets it through to a real handler mismatch.
const initRequestBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"probe","version":"0"}}}`

// TestHTTPTokenPathRouting: only the exact /{token}/mcp route reaches the
// SDK handler. The old /mcp, a wrong token, and the bare root all die in the
// ServeMux default 404 — that 404 IS the authentication failure surface (the
// token never appears in any response or log).
func TestHTTPTokenPathRouting(t *testing.T) {
	ts, _ := startHTTPTestServer(t)

	probes := []struct {
		name   string
		method string
		path   string
	}{
		{"POST old /mcp (no token)", http.MethodPost, "/mcp"},
		{"POST wrong token", http.MethodPost, "/wrong-token/mcp"},
		{"POST empty token", http.MethodPost, "//mcp"},
		{"GET bare root", http.MethodGet, "/"},
	}
	for _, p := range probes {
		req, err := http.NewRequestWithContext(context.Background(), p.method, ts.URL+p.path, strings.NewReader(initRequestBody))
		if err != nil {
			t.Fatalf("%s: build request: %v", p.name, err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: do request: %v", p.name, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", p.name, resp.StatusCode)
		}
	}
}

// TestHTTPDefaultInstructions: the initialize handshake against /{token}/mcp
// succeeds and the result carries the default instructions — a single
// sentence naming the actual allowed root (per-tool semantics live only in
// the tool descriptions, not here). One not-contains pins that the old
// operational wording (the read-first rule, now description territory)
// stays out of the text every session pays context for.
func TestHTTPDefaultInstructions(t *testing.T) {
	ts, dir := startHTTPTestServer(t)
	cs := connectHTTP(t, ts)

	res := cs.InitializeResult()
	if res == nil {
		t.Fatal("InitializeResult = nil after connect")
	}
	inst := res.Instructions
	if !strings.Contains(inst, "under: "+dir) {
		t.Errorf("instructions do not name the allowed root %q:\n%s", dir, inst)
	}
	if strings.Contains(inst, "requires reading that file in the same session first") {
		t.Errorf("instructions still carry the old read-first sentence (tool-description territory):\n%s", inst)
	}
}

// TestHTTPInstructionsOverride: a non-empty FILE_EDIT_MCP_INSTRUCTIONS wins
// verbatim over the default — the operator override path.
func TestHTTPInstructionsOverride(t *testing.T) {
	const override = "operator override: this vault is a test fixture"
	t.Setenv(instructionsEnv, override)
	ts, _ := startHTTPTestServer(t)
	cs := connectHTTP(t, ts)

	res := cs.InitializeResult()
	if res == nil {
		t.Fatal("InitializeResult = nil after connect")
	}
	if got := res.Instructions; got != override {
		t.Errorf("instructions = %q, want the verbatim override %q", got, override)
	}
}
