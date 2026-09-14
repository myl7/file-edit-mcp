// HTTP transport tests (ARCHITECTURE §0/§9): the real StreamableHTTP wiring
// from main.go served by httptest and driven by the SDK's StreamableHTTP
// client — one write→read E2E round trip, the per-connection session
// isolation (a read on connection A must not unlock an edit on connection
// B; a cross-connection concurrent write must surface as EStaleRead on the
// connection whose marker went stale), the token-path routing (everything
// but /{token}/mcp is a mux 404), the initialize-result instructions, the
// MCP-Protocol-Version downgrade for the sessionless-protocol header, the
// header/_meta sync rewrite that keeps 2026-07-28 clients (the ChatGPT
// connector) past the SDK's -32020 mismatch, and the direct server/discover
// responder that steers them back onto the legacy initialize handshake.

package main

import (
	"context"
	"encoding/json"
	"fmt"
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

// discoverRequestBody is the server/discover probe a 2026-07-28 client (the
// ChatGPT connector) sends: ONE request carrying BOTH the sessionless
// protocol's MCP-Protocol-Version header (set by the caller) and its
// per-request _meta version, plus the client identity keys the new protocol
// stamps. With the header-only rewrite this shape died in -32020
// (header 2025-11-25 vs _meta 2026-07-28) — the bug the discover responder
// and the sync rewrite below exist to kill.
const discoverRequestBody = `{"jsonrpc":"2.0","id":41,"method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"io.modelcontextprotocol/clientInfo":{"name":"chatgpt-connector","version":"1"}}}}`

// wantDiscoverVersions is the discover answer's supportedVersions: exactly
// the stateful set, newest first, and pointedly WITHOUT 2026-07-28 — that
// omission is the downgrade signal that steers a 2026-07-28 client back to
// the legacy initialize handshake (official go-sdk protocol docs).
var wantDiscoverVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

// TestHTTPDiscoverResponse: the single-message discover probe gets a direct
// 200 answer (never an SDK -32601/-32020) with the four stateful versions,
// capabilities.tools, and the same instructions text initialize sends. Both
// framings are pinned: one SSE message frame when Accept lists
// text/event-stream, raw JSON when it does not.
func TestHTTPDiscoverResponse(t *testing.T) {
	ts, dir := startHTTPTestServer(t)
	endpoint := ts.URL + "/" + testToken + "/mcp"

	for _, tc := range []struct {
		name   string
		accept string
		sse    bool
	}{
		{"SSE frame when Accept lists text/event-stream", "application/json, text/event-stream", true},
		{"plain JSON when Accept is application/json only", "application/json", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, endpoint, strings.NewReader(discoverRequestBody))
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", tc.accept)
			req.Header.Set(mcpProtocolVersionHeader, "2026-07-28")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("do request: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("discover: status = %d, want 200", resp.StatusCode)
			}
			b, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}

			var payload []byte
			if tc.sse {
				if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
					t.Fatalf("Content-Type = %q, want text/event-stream", ct)
				}
				var data string
				for _, line := range strings.Split(string(b), "\n") {
					if v, ok := strings.CutPrefix(line, "data:"); ok {
						data = strings.TrimSpace(v)
					}
				}
				if !strings.Contains(string(b), "event: message") || data == "" {
					t.Fatalf("discover: no SSE message frame:\n%s", b)
				}
				payload = []byte(data)
			} else {
				if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
					t.Fatalf("Content-Type = %q, want application/json", ct)
				}
				payload = b
			}

			var frame struct {
				JSONRPC string `json:"jsonrpc"`
				ID      int    `json:"id"`
				Result  struct {
					SupportedVersions []string `json:"supportedVersions"`
					Capabilities      struct {
						Tools map[string]any `json:"tools"`
					} `json:"capabilities"`
					Instructions string `json:"instructions"`
				} `json:"result"`
			}
			if err := json.Unmarshal(payload, &frame); err != nil {
				t.Fatalf("discover: decode payload: %v\n%s", err, payload)
			}
			if frame.JSONRPC != "2.0" || frame.ID != 41 {
				t.Errorf("envelope = jsonrpc %q id %d, want 2.0 echoing id 41", frame.JSONRPC, frame.ID)
			}
			if !slices.Equal(frame.Result.SupportedVersions, wantDiscoverVersions) {
				t.Errorf("supportedVersions = %v, want exactly %v (newest first)", frame.Result.SupportedVersions, wantDiscoverVersions)
			}
			if strings.Contains(string(payload), "2026-07-28") {
				t.Errorf("discover answer must not offer 2026-07-28 (it is the downgrade signal):\n%s", payload)
			}
			if frame.Result.Capabilities.Tools == nil {
				t.Errorf("capabilities.tools missing from discover answer:\n%s", payload)
			}
			if !strings.Contains(frame.Result.Instructions, "under: "+dir) {
				t.Errorf("instructions do not name the allowed root %q:\n%s", dir, frame.Result.Instructions)
			}
		})
	}
}

// TestHTTPDiscoverInBatch: a batch carrying server/discover among other
// messages is NOT intercepted (the responder only consumes single requests
// — a batch cannot be split without stranding siblings) and must not die at
// the HTTP level: the sync rewrite downgrades the out-of-set header and
// _meta versions together, so no 400 and no -32020, and the SDK answers
// in-band — -32601 for the discover element (a stateful server has no
// discover), a normal result for the logging/setLevel sibling.
func TestHTTPDiscoverInBatch(t *testing.T) {
	ts, _ := startHTTPTestServer(t)
	endpoint := ts.URL + "/" + testToken + "/mcp"

	body := `[` +
		`{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}},` +
		`{"jsonrpc":"2.0","id":2,"method":"logging/setLevel","params":{"level":"info","_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}` +
		`]`
	resp := httpMCPPost(t, endpoint, body, "", "2026-07-28")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("batch with discover: status = %d, want 200 (in-band per-message answers, not an HTTP-level rejection)", resp.StatusCode)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if strings.Contains(string(b), "-32020") {
		t.Errorf("batch answer carries -32020 (header/_meta mismatch):\n%s", b)
	}

	// One outcome per request id in the batch.
	outcomes := map[int]struct {
		errCode  int
		hasError bool
		result   json.RawMessage
	}{}
	for _, line := range strings.Split(string(b), "\n") {
		v, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		var frame struct {
			ID    int `json:"id"`
			Error *struct {
				Code int `json:"code"`
			} `json:"error"`
			Result json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(v)), &frame); err != nil {
			t.Fatalf("decode frame: %v\n%s", err, v)
		}
		o := outcomes[frame.ID]
		o.result = frame.Result
		if frame.Error != nil {
			o.hasError = true
			o.errCode = frame.Error.Code
		}
		outcomes[frame.ID] = o
	}
	if len(outcomes) != 2 {
		t.Fatalf("batch answer frames = %d, want one per message (2):\n%s", len(outcomes), b)
	}
	if o := outcomes[1]; !o.hasError || o.errCode != -32601 {
		t.Errorf("discover element outcome = (err=%v code=%d), want in-band -32601:\n%s", o.hasError, o.errCode, b)
	}
	if o := outcomes[2]; o.hasError || o.result == nil {
		t.Errorf("logging/setLevel element outcome = (err=%v result=%s), want a normal result:\n%s", o.hasError, o.result, b)
	}
}

// The post-downgrade traffic of a 2026-07-28 client: header AND params._meta
// stamped 2026-07-28 on every request — the shape the ChatGPT connector
// keeps sending after the discover answer steers it onto the legacy
// handshake, and the one the sync rewrite (not the header-only rule) must
// keep alive.
const (
	initializedMetaBody = `{"jsonrpc":"2.0","method":"notifications/initialized","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}`
	toolsListMetaBody   = `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}`
)

// TestHTTPMetaVersionSyncChain pins the header+_meta sync rewrite
// end-to-end: initialize goes out plain (the legacy handshake the discover
// answer chose), then notifications/initialized and tools/list each carry
// consistent 2026-07-28 header AND _meta signals — both must be downgraded
// to the legacy shape (key stripped, out-of-set header removed) so the pair
// can never trip the SDK's -32020 mismatch nor its stateless-only 400 for
// _meta-carrying messages. 202 for the notification, 200 with the full tool
// set for the call.
func TestHTTPMetaVersionSyncChain(t *testing.T) {
	ts, _ := startHTTPTestServer(t)
	endpoint := ts.URL + "/" + testToken + "/mcp"

	sessionID := openRawSession(t, endpoint, "")

	resp := httpMCPPost(t, endpoint, initializedMetaBody, sessionID, "2026-07-28")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("notifications/initialized: status = %d, want 202", resp.StatusCode)
	}

	resp = httpMCPPost(t, endpoint, toolsListMetaBody, sessionID, "2026-07-28")
	if names := decodeToolsListResponse(t, resp); !slices.Equal(names, wantToolNames) {
		t.Errorf("tools/list tools = %v, want %v", names, wantToolNames)
	}
}

// TestHTTPVersionRewriteBodyPassthrough: requests without a per-request
// _meta version are forwarded byte-identically — the buffering middleware
// must not corrupt or even re-encode such bodies. The second case is the
// trap shape: the body merely MENTIONS the _meta key as data inside a tool
// argument (substring hit, no structural params._meta match → still no
// rewrite), and the first carries multi-byte UTF-8 both paths must round-trip
// untouched. The header-only rule keeps applying in both cases.
func TestHTTPVersionRewriteBodyPassthrough(t *testing.T) {
	bodies := []string{
		`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"write","arguments":{"file_path":"/tmp/unicodé-世界.txt","content":"héllo → 世界 ✓"}}}`,
		`{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"write","arguments":{"file_path":"/tmp/x.txt","content":"mentions \"io.modelcontextprotocol/protocolVersion\" as data"}}}`,
	}
	for _, body := range bodies {
		var gotBody []byte
		var gotHeader string
		inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotBody, _ = io.ReadAll(r.Body)
			gotHeader = r.Header.Get(mcpProtocolVersionHeader)
		})
		req := httptest.NewRequest(http.MethodPost, "/"+testToken+"/mcp", strings.NewReader(body))
		req.Header.Set(mcpProtocolVersionHeader, "2026-07-28")
		versionRewrite(inner).ServeHTTP(httptest.NewRecorder(), req)
		if string(gotBody) != body {
			t.Errorf("body not byte-identical through versionRewrite:\n got %s\nwant %s", gotBody, body)
		}
		if gotHeader != "2025-11-25" {
			t.Errorf("inner header = %q, want the header-only rewrite to 2025-11-25", gotHeader)
		}
	}
}

// TestHTTPUnicodeBodyRoundTrip: one full-stack tools/call write whose body
// carries multi-byte UTF-8 content, through the buffered-restore rewrite in
// the chain — the bytes on disk must be exactly the content the JSON
// carried, pinning that body buffering never corrupts unicode bodies.
func TestHTTPUnicodeBodyRoundTrip(t *testing.T) {
	ts, dir := startHTTPTestServer(t)
	endpoint := ts.URL + "/" + testToken + "/mcp"
	sessionID := openRawSession(t, endpoint, "")

	resp := httpMCPPost(t, endpoint, initializedNotificationBody, sessionID, "")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("notifications/initialized: status = %d, want 202", resp.StatusCode)
	}

	const content = "héllo → 世界 ✓"
	path := filepath.Join(dir, "unicode.txt")
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"write","arguments":{"file_path":%q,"content":%q}}}`, path, content)
	resp = httpMCPPost(t, endpoint, body, sessionID, "2026-07-28")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tools/call write: status = %d, want 200", resp.StatusCode)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != content {
		t.Errorf("disk = %q (%v), want exactly the unicode content %q", b, err, content)
	}
}
