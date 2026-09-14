// HTTP transport tests (ARCHITECTURE §0/§9): the real stateless
// StreamableHTTP wiring from main.go served by httptest and driven two
// ways — by the SDK's StreamableHTTP client (initialize handshake, one
// write→read E2E round trip, instructions coverage) and by raw POSTs in the
// exact shapes the 2026-07-28 protocol (SEP-2567, the stateless handshake
// ChatGPT custom connectors require) prescribes: server/discover and
// header-stamped tools/call. Plus the marker semantics in both scopes the
// token-as-session model defines (within one token: one read anywhere
// unlocks that token's writes across all its stateless requests; across
// tokens: markers are isolated — the never-read EStaleRead variant — while
// any writer stales every other token's marker — the stale variant; a
// never-read file stays fenced off with the never-read variant) and the
// token-path routing (everything but /{token}/mcp is a mux 404).

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
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/myl7/file-edit-mcp/internal/tools"
)

// testToken is the fixed token the single-token HTTP tests configure via
// tokenEnv — the same loadTokens+newMCPHandler path run() takes (the list
// var stays unset, so loadTokens exercises its single-var fallback), never
// a hand-built mux. The multi-token tests use their own tokens and the
// list-form harness below.
const testToken = "test-token-1"

// The wire headers of the >= 2026-07-28 stateless protocol (SEP-2243): the
// protocol-version header every request carries, and the Mcp-Method /
// Mcp-Name headers the SDK enforces on this endpoint for 2026-07-28
// traffic. The SDK client behind connectHTTP stamps its own; the raw-shape
// tests below must control the exact bytes, so the names live here.
const (
	protoVersionHeader = "MCP-Protocol-Version"
	methodHeader       = "Mcp-Method"
	nameHeader         = "Mcp-Name"
)

// statelessProtocolVersion is the sessionless protocol date (SEP-2567) the
// stateless handler serves natively — the version ChatGPT connectors
// discover with and never fall back from.
const statelessProtocolVersion = "2026-07-28"

// startHTTPTestServer boots the production single-token HTTP assembly
// (loadTokens over tokenEnv + newMCPHandler) over one fresh temp allowed
// dir, exactly as run() and runHTTP do minus the http.Server/keepalive
// plumbing. The list var is left unset on purpose: these tests pin the
// single-var fallback form and stay untouched by the multi-token work; the
// list-form twin is startMultiTokenHTTPTestServer.
func startHTTPTestServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", dir, err)
	}
	t.Setenv(tokenEnv, testToken)
	t.Setenv(tokensEnv, "")
	tokens, err := loadTokens()
	if err != nil {
		t.Fatalf("loadTokens: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	shared, err := tools.NewShared([]string{dir}, logger)
	if err != nil {
		t.Fatalf("tools.NewShared: %v", err)
	}
	ts := httptest.NewServer(newMCPHandler(shared, logger, tokens))
	t.Cleanup(ts.Close)
	return ts, real
}

// startMultiTokenHTTPTestServer is the list-form twin of
// startHTTPTestServer: tokens are configured through tokensEnv (comma
// join, exactly what an operator would write) and resolved by loadTokens,
// so every multi-token test runs the same env-parse + per-token route/Conn
// construction run() performs. tokenEnv is set to empty so a passing test
// proves the list form works alone, not on top of the single var.
func startMultiTokenHTTPTestServer(t *testing.T, tokens ...string) (*httptest.Server, string) {
	t.Helper()
	if len(tokens) < 2 {
		t.Fatalf("multi-token harness wants >=2 tokens, got %v", tokens)
	}
	dir := t.TempDir()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", dir, err)
	}
	t.Setenv(tokensEnv, strings.Join(tokens, ","))
	t.Setenv(tokenEnv, "")
	got, err := loadTokens()
	if err != nil {
		t.Fatalf("loadTokens: %v", err)
	}
	if !slices.Equal(got, tokens) {
		t.Fatalf("loadTokens = %v, want %v", got, tokens)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	shared, err := tools.NewShared([]string{dir}, logger)
	if err != nil {
		t.Fatalf("tools.NewShared: %v", err)
	}
	ts := httptest.NewServer(newMCPHandler(shared, logger, got))
	t.Cleanup(ts.Close)
	return ts, real
}

// connectHTTP opens one MCP client session against ts at the shared
// testToken endpoint — the single-token tests' convenience form.
func connectHTTP(t *testing.T, ts *httptest.Server) *mcp.ClientSession {
	t.Helper()
	return connectHTTPToken(t, ts, testToken)
}

// connectHTTPToken opens one MCP client session against ts at the given
// token's endpoint. Against the stateless server a "session" is purely
// client-side bookkeeping: the initialize response carries no
// Mcp-Session-Id (stateless never issues one; the SDK client tolerates
// that), and every request is served by a fresh per-request server over
// the token's dedicated tools.Conn — per-token marker scope (see
// tools.Conn) with cross-request persistence within the token. The
// endpoint carries the token in the path, as production clients must.
func connectHTTPToken(t *testing.T, ts *httptest.Server, token string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "http-test-client", Version: "1.0"}, nil)
	cs, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: ts.URL + "/" + token + "/mcp",
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

// TestHTTPWriteReadE2E: one client session, write a new file then read it
// back. Under stateless serving these are two independent requests through
// two fresh per-request servers — the round trip works only because the
// process-wide Conn carries the write's mark-known across them (better than
// the old per-session state ever needed to).
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

// TestHTTPProcessWideMarkers pins the approved marker semantics of the
// stateless transport: the read-before-write markers live on ONE Conn for
// the whole process, so client-session boundaries no longer isolate them.
// A's read unlocks B's edit (the never-read EStaleRead variant means "not
// read anywhere in this process"); a change the process never saw (an
// out-of-band disk write — anything this process wrote refreshed the
// marker, because then the content is known) is still the stale EStaleRead
// variant; a file no request ever read stays fenced off with the never-read
// variant. The old TestHTTPSessionIsolation asserted the opposite first
// arm; that isolation is deliberately gone (see tools.Conn).
func TestHTTPProcessWideMarkers(t *testing.T) {
	ts, dir := startHTTPTestServer(t)
	a := connectHTTP(t, ts)
	b := connectHTTP(t, ts)
	path := filepath.Join(dir, "shared.txt")
	if err := os.WriteFile(path, []byte("v0\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A reads; the marker lands on the process-wide Conn.
	if text, isErr := httpCallTool(t, a, "read", map[string]any{"file_path": path}); isErr {
		t.Fatalf("A read: unexpected error: %s", text)
	}

	// B edits without any read of its own: SUCCEEDS — A's read is process-
	// wide. (This exact call hit the never-read EStaleRead variant under
	// per-session markers.)
	if text, isErr := httpCallTool(t, b, "edit", map[string]any{
		"file_path": path, "old_string": "v0", "new_string": "from B",
	}); isErr {
		t.Fatalf("B edit after A's read: unexpected error: %s", text)
	}
	if got, _ := os.ReadFile(path); string(got) != "from B\n" {
		t.Fatalf("disk = %q, want B's edit applied", got)
	}

	// An out-of-band change (a host-side editor, anything but this process):
	// no request of ours saw it, so the process marker is stale and A's edit
	// is rejected EStaleRead instead of silently overwriting it.
	if err := os.WriteFile(path, []byte("out-of-band\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	text, isErr := httpCallTool(t, a, "edit", map[string]any{
		"file_path": path, "old_string": "out-of-band", "new_string": "from A",
	})
	if !isErr || !strings.Contains(text, "file changed since last read") {
		t.Fatalf("A edit after out-of-band change: (isErr=%v) %q, want EStaleRead", isErr, text)
	}
	if got, _ := os.ReadFile(path); string(got) != "out-of-band\n" {
		t.Errorf("disk = %q, want the out-of-band content untouched by the rejected edit", got)
	}

	// Recovery: A re-reads (fresh process marker), and A's edit now lands.
	if text, isErr := httpCallTool(t, a, "read", map[string]any{"file_path": path}); isErr {
		t.Fatalf("A re-read: unexpected error: %s", text)
	}
	if text, isErr := httpCallTool(t, a, "edit", map[string]any{
		"file_path": path, "old_string": "out-of-band", "new_string": "from A",
	}); isErr {
		t.Fatalf("A edit after re-read: unexpected error: %s", text)
	}
	if got, _ := os.ReadFile(path); string(got) != "from A\n" {
		t.Errorf("disk = %q, want A's edit after the re-read", got)
	}

	// A never-read existing file stays fenced off for everyone: the
	// never-read EStaleRead variant.
	unseen := filepath.Join(dir, "unseen.txt")
	if err := os.WriteFile(unseen, []byte("never read\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	text, isErr = httpCallTool(t, b, "write", map[string]any{
		"file_path": unseen, "content": "x\n",
	})
	if !isErr || !strings.Contains(text, "has not been read in this session; read it before writing") {
		t.Fatalf("write to never-read file: (isErr=%v) %q, want the never-read EStaleRead variant", isErr, text)
	}
}

// TestHTTPWriteReadEditAcrossRequests pins same-token cross-request marker
// persistence in its minimal full shape: write→read→edit as THREE separate
// stateless requests under one token. Each request runs through its own
// fresh per-request server; the edit only lands because the token's Conn
// carries the write's and the read's markers across the request boundaries
// (TestHTTPWriteReadE2E covers the write→read prefix; this adds the edit).
func TestHTTPWriteReadEditAcrossRequests(t *testing.T) {
	ts, dir := startHTTPTestServer(t)
	cs := connectHTTP(t, ts)
	path := filepath.Join(dir, "three-requests.txt")

	if text, isErr := httpCallTool(t, cs, "write", map[string]any{
		"file_path": path, "content": "one\ntwo\n",
	}); isErr {
		t.Fatalf("write: unexpected tool error: %s", text)
	}
	if text, isErr := httpCallTool(t, cs, "read", map[string]any{"file_path": path}); isErr {
		t.Fatalf("read: unexpected tool error: %s", text)
	}
	if text, isErr := httpCallTool(t, cs, "edit", map[string]any{
		"file_path": path, "old_string": "two", "new_string": "TWO",
	}); isErr {
		t.Fatalf("edit: unexpected tool error: %s", text)
	}
	if got, _ := os.ReadFile(path); string(got) != "one\nTWO\n" {
		t.Errorf("disk = %q, want the edit applied", got)
	}
}

// TestHTTPMultiTokenRouting: every configured token gets a working endpoint
// — the initialize handshake succeeds on each (connectHTTPToken performs
// it) and each route answers a real tool call over its own Conn — and an
// unknown token still dies in the plain mux 404: route existence is the
// whole auth story, for every token equally.
func TestHTTPMultiTokenRouting(t *testing.T) {
	tokens := []string{"multi-token-a", "multi-token-b", "multi-token-c"}
	ts, dir := startMultiTokenHTTPTestServer(t, tokens...)
	if err := os.WriteFile(filepath.Join(dir, "probe.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tok := range tokens {
		cs := connectHTTPToken(t, ts, tok)
		if res := cs.InitializeResult(); res == nil {
			t.Errorf("token %q: InitializeResult = nil after connect", tok)
			continue
		}
		// A real read-only tool call through that token's route: glob over
		// the temp dir must list the seeded file.
		text, isErr := httpCallTool(t, cs, "glob", map[string]any{"pattern": "*", "path": dir})
		if isErr || !strings.Contains(text, "probe.txt") {
			t.Errorf("token %q: glob = (isErr=%v) %q, want probe.txt listed", tok, isErr, text)
		}
	}

	// Unknown token: still the plain ServeMux 404, never an SDK answer.
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL+"/no-such-token/mcp", strings.NewReader(initRequestBody))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("unknown-token probe: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown token: status = %d, want 404", resp.StatusCode)
	}
}

// TestHTTPCrossTokenIsolation is the headline token-as-session test: two
// tokens, two dedicated Conns, one file — the cross-client isolation the
// stateless protocol removed, restored along the credential axis. The
// never-read EStaleRead variant is per token (A's read does not authorize
// B's edit); ANY writer stales every other token's marker because the
// stale variant compares against the file's CURRENT state (B's successful
// edit leaves A's marker stale → A gets the stale variant; an out-of-band
// disk write stales both); a never-read file is fenced off for both tokens;
// each re-read re-arms only that token.
func TestHTTPCrossTokenIsolation(t *testing.T) {
	const tokA, tokB = "iso-token-a", "iso-token-b"
	ts, dir := startMultiTokenHTTPTestServer(t, tokA, tokB)
	a := connectHTTPToken(t, ts, tokA)
	b := connectHTTPToken(t, ts, tokB)
	path := filepath.Join(dir, "cross.txt")
	if err := os.WriteFile(path, []byte("v0\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A reads F: the marker lands on token A's Conn only.
	if text, isErr := httpCallTool(t, a, "read", map[string]any{"file_path": path}); isErr {
		t.Fatalf("A read: unexpected error: %s", text)
	}

	// B's edit without a read of its own: the never-read EStaleRead variant —
	// A's read does not authorize B (the exact call that SUCCEEDS under one
	// shared Conn in TestHTTPProcessWideMarkers, fenced again now that
	// markers are per token).
	text, isErr := httpCallTool(t, b, "edit", map[string]any{
		"file_path": path, "old_string": "v0", "new_string": "from B",
	})
	if !isErr || !strings.Contains(text, "has not been read in this session; read it before writing") {
		t.Fatalf("B edit after A's read: (isErr=%v) %q, want the never-read EStaleRead variant", isErr, text)
	}

	// B reads F, then B's edit succeeds.
	if text, isErr := httpCallTool(t, b, "read", map[string]any{"file_path": path}); isErr {
		t.Fatalf("B read: unexpected error: %s", text)
	}
	if text, isErr := httpCallTool(t, b, "edit", map[string]any{
		"file_path": path, "old_string": "v0", "new_string": "from B",
	}); isErr {
		t.Fatalf("B edit after B's read: unexpected error: %s", text)
	}
	if got, _ := os.ReadFile(path); string(got) != "from B\n" {
		t.Fatalf("disk = %q, want B's edit applied", got)
	}

	// A's marker predates B's write: A's edit is EStaleRead — never a silent
	// overwrite of what B just landed.
	text, isErr = httpCallTool(t, a, "edit", map[string]any{
		"file_path": path, "old_string": "from B", "new_string": "from A",
	})
	if !isErr || !strings.Contains(text, "file changed since last read") {
		t.Fatalf("A edit after B's write: (isErr=%v) %q, want EStaleRead", isErr, text)
	}

	// Recovery for A: re-read (fresh marker on A's Conn), then edit.
	if text, isErr := httpCallTool(t, a, "read", map[string]any{"file_path": path}); isErr {
		t.Fatalf("A re-read: unexpected error: %s", text)
	}
	if text, isErr := httpCallTool(t, a, "edit", map[string]any{
		"file_path": path, "old_string": "from B", "new_string": "from A",
	}); isErr {
		t.Fatalf("A edit after re-read: unexpected error: %s", text)
	}
	if got, _ := os.ReadFile(path); string(got) != "from A\n" {
		t.Fatalf("disk = %q, want A's edit applied", got)
	}

	// A never-read existing file stays fenced off for BOTH tokens with the
	// never-read EStaleRead variant.
	unseen := filepath.Join(dir, "unseen-cross.txt")
	if err := os.WriteFile(unseen, []byte("never read\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, cs := range map[string]*mcp.ClientSession{"A": a, "B": b} {
		text, isErr = httpCallTool(t, cs, "write", map[string]any{
			"file_path": unseen, "content": "x\n",
		})
		if !isErr || !strings.Contains(text, "has not been read in this session; read it before writing") {
			t.Errorf("token %s write to never-read file: (isErr=%v) %q, want the never-read EStaleRead variant", name, isErr, text)
		}
	}

	// An out-of-band disk write (a host-side editor, anything but this
	// process) stales BOTH tokens' markers: each Conn compares its own
	// last-seen state against the moved file, so each must reject with
	// EStaleRead rather than silently overwrite.
	if err := os.WriteFile(path, []byte("out-of-band\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, cs := range map[string]*mcp.ClientSession{"A": a, "B": b} {
		text, isErr = httpCallTool(t, cs, "edit", map[string]any{
			"file_path": path, "old_string": "out-of-band", "new_string": "from " + name,
		})
		if !isErr || !strings.Contains(text, "file changed since last read") {
			t.Errorf("token %s edit after out-of-band change: (isErr=%v) %q, want EStaleRead", name, isErr, text)
		}
	}
	if got, _ := os.ReadFile(path); string(got) != "out-of-band\n" {
		t.Errorf("disk = %q, want the out-of-band content untouched by the rejected edits", got)
	}

	// Until each re-reads: B first (its fresh marker arms its edit), then A
	// (B's edit moved the file again, so A needs its own re-read).
	if text, isErr := httpCallTool(t, b, "read", map[string]any{"file_path": path}); isErr {
		t.Fatalf("B re-read: unexpected error: %s", text)
	}
	if text, isErr := httpCallTool(t, b, "edit", map[string]any{
		"file_path": path, "old_string": "out-of-band", "new_string": "from B again",
	}); isErr {
		t.Fatalf("B edit after re-read: unexpected error: %s", text)
	}
	if text, isErr := httpCallTool(t, a, "read", map[string]any{"file_path": path}); isErr {
		t.Fatalf("A re-read: unexpected error: %s", text)
	}
	if text, isErr := httpCallTool(t, a, "edit", map[string]any{
		"file_path": path, "old_string": "from B again", "new_string": "from A again",
	}); isErr {
		t.Fatalf("A edit after re-read: unexpected error: %s", text)
	}
	if got, _ := os.ReadFile(path); string(got) != "from A again\n" {
		t.Errorf("disk = %q, want A's edit after both tokens re-read", got)
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

// statelessDiscoverBody is the exact discover request shape verified
// against the stateless server (the ChatGPT connector's probe): a single
// server/discover POST whose params._meta carries the protocol version and
// client capabilities, with MCP-Protocol-Version and Mcp-Method headers
// stamped alongside (set by httpMCPPost below).
const statelessDiscoverBody = `{"jsonrpc":"2.0","id":41,"method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`

// TestHTTPDiscoverStateless: the stateless SDK handler answers
// server/discover natively — full correct shape, no middleware of ours —
// advertising 2026-07-28 (the version that steers ChatGPT connectors onto
// the sessionless protocol instead of into a legacy-initialize fallback
// their client does not implement) and the same instructions text
// initialize sends.
func TestHTTPDiscoverStateless(t *testing.T) {
	ts, dir := startHTTPTestServer(t)
	endpoint := ts.URL + "/" + testToken + "/mcp"

	resp := httpMCPPost(t, endpoint, statelessDiscoverBody, map[string]string{
		protoVersionHeader: statelessProtocolVersion,
		methodHeader:       "server/discover",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("discover: status = %d, want 200", resp.StatusCode)
	}
	payload := lastSSEData(t, resp)

	var frame struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Result  struct {
			SupportedVersions []string `json:"supportedVersions"`
			Instructions      string   `json:"instructions"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(payload), &frame); err != nil {
		t.Fatalf("discover: decode payload: %v\n%s", err, payload)
	}
	if frame.JSONRPC != "2.0" || frame.ID != 41 {
		t.Errorf("envelope = jsonrpc %q id %d, want 2.0 echoing id 41", frame.JSONRPC, frame.ID)
	}
	if !slices.Contains(frame.Result.SupportedVersions, statelessProtocolVersion) {
		t.Errorf("supportedVersions = %v, want it to offer 2026-07-28 (the stateless handshake ChatGPT connectors need):\n%s", frame.Result.SupportedVersions, payload)
	}
	if !strings.Contains(frame.Result.Instructions, "under: "+dir) {
		t.Errorf("instructions do not name the allowed root %q:\n%s", dir, frame.Result.Instructions)
	}
}

// TestHTTPStatelessToolsCallHeaders pins the header-enforced tools/call
// flow a 2026-07-28 client uses (SEP-2243): Mcp-Method + Mcp-Name headers
// beside the _meta version, answered by a real tool result. The second arm
// pins the enforcement itself on this endpoint: the same request WITHOUT
// Mcp-Name dies with HTTP 400 (SDK CodeHeaderMismatch), which is exactly
// the contract the header-carrying clients rely on.
func TestHTTPStatelessToolsCallHeaders(t *testing.T) {
	ts, dir := startHTTPTestServer(t)
	endpoint := ts.URL + "/" + testToken + "/mcp"
	if err := os.WriteFile(filepath.Join(dir, "marker.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// glob "*" over the temp dir: echo-style, read-only, and its result must
	// name the seeded file.
	body := `{"jsonrpc":"2.0","id":42,"method":"tools/call","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}},"name":"glob","arguments":{"pattern":"*","path":` + jsonQuote(dir) + `}}}`
	resp := httpMCPPost(t, endpoint, body, map[string]string{
		protoVersionHeader: statelessProtocolVersion,
		methodHeader:       "tools/call",
		nameHeader:         "glob",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tools/call glob: status = %d, want 200", resp.StatusCode)
	}
	payload := lastSSEData(t, resp)
	if !strings.Contains(payload, "marker.txt") {
		t.Errorf("glob result does not list marker.txt:\n%s", payload)
	}
	var frame struct {
		ID     int `json:"id"`
		Result struct {
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(payload), &frame); err != nil {
		t.Fatalf("tools/call: decode payload: %v\n%s", err, payload)
	}
	if frame.ID != 42 || frame.Result.IsError {
		t.Errorf("tools/call frame = id %d isError %v, want id 42 with a clean result:\n%s", frame.ID, frame.Result.IsError, payload)
	}

	// Same request minus Mcp-Name: the SDK must refuse it at the HTTP level.
	resp = httpMCPPost(t, endpoint, body, map[string]string{
		protoVersionHeader: statelessProtocolVersion,
		methodHeader:       "tools/call",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("tools/call without Mcp-Name: status = %d, want 400 (header enforcement)", resp.StatusCode)
	}
}

// httpMCPPost issues one MCP POST against endpoint with the headers every
// raw test needs (JSON content type, the dual Accept the stateless handler
// requires) plus hdr for the per-request extras (protocol-version and
// SEP-2243 method/name headers). Raw requests on purpose: the SDK client
// transport stamps its own negotiated headers, which is exactly the input
// these tests must control.
func httpMCPPost(t *testing.T, endpoint, body string, hdr map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, endpoint, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

// lastSSEData reads an SSE response body and returns the last data-frame
// payload: the answer frame for a single-request POST (any earlier frames
// would be notification noise, which a plain request never produces here).
func lastSSEData(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var last string
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "data:"); ok {
			last = strings.TrimSpace(v)
		}
	}
	if last == "" {
		t.Fatalf("no SSE data frame in response:\n%s", b)
	}
	return last
}

// jsonQuote renders s as a JSON string literal (the one place the raw
// bodies above need a runtime-interpolated value: the temp dir path).
func jsonQuote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err) // unreachable: marshaling a string
	}
	return string(b)
}
