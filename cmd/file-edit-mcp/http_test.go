// HTTP transport tests (ARCHITECTURE §0/§9): the real StreamableHTTP wiring
// from main.go served by httptest and driven by the SDK's StreamableHTTP
// client — one write→read E2E round trip, the per-connection session
// isolation (a read on connection A must not unlock an edit on connection
// B; a cross-connection concurrent write must surface as EStaleRead on the
// connection whose marker went stale), the token-path routing (everything
// but /{token}/mcp is a mux 404), the initialize-result instructions, and
// the MCP-Protocol-Version downgrade for the sessionless-protocol header.

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

// initializedNotificationBody and toolsListRequestBody extend the raw-probe
// bodies below the handshake: the initialized notification (HTTP 202, no
// result) and a tools/list call whose answer must come back as an SSE
// result frame.
const (
	initializedNotificationBody = `{"jsonrpc":"2.0","method":"notifications/initialized"}`
	toolsListRequestBody        = `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`
)

// httpMCPPost issues one MCP POST against endpoint with the headers a real
// client sends: JSON content type, the dual Accept, the session id once the
// handshake produced one, and protoVersion as MCP-Protocol-Version ("" =
// omit the header — the pre-handshake shape clients use for initialize).
// Raw requests on purpose: the SDK client transport stamps its own
// negotiated header, which is exactly the input these tests must control.
func httpMCPPost(t *testing.T, endpoint, body, sessionID, protoVersion string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, endpoint, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	if protoVersion != "" {
		req.Header.Set(mcpProtocolVersionHeader, protoVersion)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

// openRawSession runs the initialize POST (carrying protoVersion as the
// MCP-Protocol-Version header, "" = none) and returns the new session id.
// The raw-handshake counterpart of connectHTTP's SDK-client path.
func openRawSession(t *testing.T, endpoint, protoVersion string) string {
	t.Helper()
	resp := httpMCPPost(t, endpoint, initRequestBody, "", protoVersion)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initialize: status = %d, want 200", resp.StatusCode)
	}
	sessionID := resp.Header.Get("Mcp-Session-Id")
	if sessionID == "" {
		t.Fatal("initialize: no Mcp-Session-Id on the response")
	}
	// Drain the InitializeResult SSE frame so the stream completes before
	// the follow-ups (the SDK closes it once the result is delivered).
	_, _ = io.ReadAll(resp.Body)
	return sessionID
}

// decodeToolsListResponse asserts resp is a 200 SSE answer to a tools/list
// call and returns the tool names from its result frame. The answer frame
// is the last data payload (any earlier frames are priming/notification
// noise, none of which a plain tools/list produces today).
func decodeToolsListResponse(t *testing.T, resp *http.Response) []string {
	t.Helper()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tools/list: status = %d, want 200", resp.StatusCode)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("tools/list: read body: %v", err)
	}
	var last string
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "data:"); ok {
			last = strings.TrimSpace(v)
		}
	}
	if last == "" {
		t.Fatalf("tools/list: no SSE data frame in response:\n%s", b)
	}
	var frame struct {
		ID     int `json:"id"`
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(last), &frame); err != nil {
		t.Fatalf("tools/list: decode frame: %v\n%s", err, last)
	}
	if frame.ID != 2 {
		t.Errorf("tools/list frame id = %d, want 2 (the request id)", frame.ID)
	}
	names := make([]string, len(frame.Result.Tools))
	for i, tool := range frame.Result.Tools {
		names[i] = tool.Name
	}
	return names
}

// wantToolNames is the full §0-§2 tool set tools/list must advertise, in
// the name order the SDK returns them (sorted).
var wantToolNames = []string{"edit", "glob", "grep", "multi_edit", "read", "write"}

// TestHTTPProtocolVersionRewriteChain: a client that upgrades to the
// sessionless 2026-07-28 header after initialize (the discover flow
// advertises the new protocol and modern clients follow — the ChatGPT web
// connector is the observed one) must still get a working session.
// initialize goes out WITHOUT the header, as clients send it; then
// notifications/initialized and tools/list each carry
// MCP-Protocol-Version: 2026-07-28, which versionRewrite downgrades to the
// stateful set before the SDK handler parses anything. Without the rewrite
// both follow-ups die in the SDK's 400 "protocol version ... is only
// supported on stateless HTTP servers" (verified against go-sdk v1.7.0,
// mcp/streamable.go serveStatefulPOST) and the connector creation fails.
func TestHTTPProtocolVersionRewriteChain(t *testing.T) {
	ts, _ := startHTTPTestServer(t)
	endpoint := ts.URL + "/" + testToken + "/mcp"

	sessionID := openRawSession(t, endpoint, "")

	// notifications/initialized WITH the sessionless header: spec answer is
	// 202 Accepted, reachable only through the rewrite.
	resp := httpMCPPost(t, endpoint, initializedNotificationBody, sessionID, "2026-07-28")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("notifications/initialized: status = %d, want 202", resp.StatusCode)
	}

	// tools/list with the same header: a result must arrive, not a 400 —
	// and it must be the full tool set.
	resp = httpMCPPost(t, endpoint, toolsListRequestBody, sessionID, "2026-07-28")
	if names := decodeToolsListResponse(t, resp); !slices.Equal(names, wantToolNames) {
		t.Errorf("tools/list tools = %v, want %v", names, wantToolNames)
	}
}

// TestHTTPAcceptedProtocolVersionChain: an accepted version passes through
// versionRewrite untouched — the full chain works when the client stamps
// the explicit 2025-06-18 header on every request, initialize included.
// Complements the rewrite test: the middleware must rewrite ONLY what a
// stateful server cannot answer (an SDK-negotiated header is a no-op pass
// through the middleware, pinned directly below).
func TestHTTPAcceptedProtocolVersionChain(t *testing.T) {
	// Middleware-level pin: an accepted version reaches the inner handler
	// verbatim, byte for byte.
	var got string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get(mcpProtocolVersionHeader)
	})
	req := httptest.NewRequest(http.MethodPost, "/"+testToken+"/mcp", nil)
	req.Header.Set(mcpProtocolVersionHeader, "2025-06-18")
	versionRewrite(inner).ServeHTTP(httptest.NewRecorder(), req)
	if got != "2025-06-18" {
		t.Errorf("inner handler saw %q, want the accepted 2025-06-18 verbatim", got)
	}

	ts, _ := startHTTPTestServer(t)
	endpoint := ts.URL + "/" + testToken + "/mcp"

	sessionID := openRawSession(t, endpoint, "2025-06-18")

	resp := httpMCPPost(t, endpoint, initializedNotificationBody, sessionID, "2025-06-18")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("notifications/initialized: status = %d, want 202", resp.StatusCode)
	}

	resp = httpMCPPost(t, endpoint, toolsListRequestBody, sessionID, "2025-06-18")
	if names := decodeToolsListResponse(t, resp); !slices.Equal(names, wantToolNames) {
		t.Errorf("tools/list tools = %v, want %v", names, wantToolNames)
	}
}

// TestRewriteProtocolVersion pins the downgrade decision table: the four
// stateful versions and the empty header pass through verbatim; the
// sessionless 2026-07-28 and anything unknown/future collapse to
// "2025-11-25", the initialize-negotiation cap. No HTTP involved — the
// table is the contract the two chain tests above exercise end to end.
func TestRewriteProtocolVersion(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""}, // pre-handshake / header omitted: SDK treats as version unknown
		{"2024-11-05", "2024-11-05"},
		{"2025-03-26", "2025-03-26"},
		{"2025-06-18", "2025-06-18"},
		{"2025-11-25", "2025-11-25"},
		{"2026-07-28", "2025-11-25"},  // sessionless-only protocol: hard 400 on stateful
		{"2027-03-18", "2025-11-25"},  // future version the SDK does not know yet
		{"garbage", "2025-11-25"},     // not a version string at all
		{"2025-06-18 ", "2025-11-25"}, // trailing space ≠ the accepted token: strict match only
	}
	for _, tc := range cases {
		if got := rewriteProtocolVersion(tc.in); got != tc.want {
			t.Errorf("rewriteProtocolVersion(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
