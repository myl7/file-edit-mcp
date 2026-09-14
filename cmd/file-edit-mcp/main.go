// Command file-edit-mcp is an MCP server exposing file read/write/edit,
// glob, and grep tools restricted to the directories allowed via --allow.
//
// Two transports (ARCHITECTURE §0): stdio (the default, one process = one
// client connection) and streamable HTTP (a resident server whose MCP
// endpoint is /{token}/mcp — token-path authentication via the
// FILE_EDIT_MCP_TOKEN environment variable; every StreamableHTTP session
// gets its own *mcp.Server and therefore its own tools.Conn session
// state). stdout carries only MCP protocol frames (handled by the SDK);
// all logging goes to stderr (§7).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/myl7/file-edit-mcp/internal/tools"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "dev"

// serverName is the MCP server name advertised during initialize.
const serverName = "file-edit-mcp"

// tokenEnv holds the HTTP transport secret (token-path authentication, §0):
// the MCP endpoint is /{token}/mcp, so knowing the URL is knowing the
// credential and nothing else is asked of the client. Required with
// --transport http; ignored with stdio (a local pipe has no auth).
const tokenEnv = "FILE_EDIT_MCP_TOKEN"

// instructionsEnv optionally overrides the initialize-result instructions
// with operator text, verbatim (escape hatch for deployment-specific context
// the default cannot carry — the default is just the allowed root paths).
// Empty/unset means default.
const instructionsEnv = "FILE_EDIT_MCP_INSTRUCTIONS"

// tokenRE bounds the token: one leading alphanumeric plus up to 127 chars
// from [A-Za-z0-9_-] (1–128 total). The charset is a strict subset of the
// URL path segment characters and contains none of ServeMux's pattern
// metacharacters ("/", "{", "}", "*"), so a validated token spliced into
// "/"+token+"/mcp" cannot alter what that pattern matches.
var tokenRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", serverName, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet(serverName, flag.ContinueOnError)
	fs.SetOutput(os.Stderr) // keep stdout free for MCP protocol frames
	var allow allowFlags
	fs.Var(&allow, "allow", "allowed directory (absolute path); repeatable, at least one required")
	transport := fs.String("transport", "stdio", "transport: stdio or http (ARCHITECTURE §0)")
	addr := fs.String("addr", ":8080", "HTTP listen address (http transport only)")
	keepalive := fs.Duration("keepalive", 4*time.Minute, "stat each allowed directory every this often to keep the CIFS automount from idle-unmounting (http transport only; 0 disables)")
	logLevel := fs.String("log-level", "info", "log level: debug, info, warn, error")
	showVer := fs.Bool("version", false, "print version and exit")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("bad flags: %w", err)
	}
	if *showVer {
		fmt.Printf("%s %s\n", serverName, version)
		return nil
	}
	if len(allow) < 1 {
		return fmt.Errorf("at least one --allow DIR is required")
	}
	if *transport != "stdio" && *transport != "http" {
		return fmt.Errorf("invalid --transport %q: want stdio or http", *transport)
	}
	// Token-path auth (§0): the HTTP route is /{token}/mcp, so the token must
	// be present and charset-valid before any mux pattern is built from it.
	// stdio deliberately never reads the variable: a local pipe has no auth.
	var token string
	if *transport == "http" {
		var err error
		if token, err = loadToken(); err != nil {
			return err
		}
	}
	if *keepalive < 0 {
		return fmt.Errorf("invalid --keepalive %s: want a non-negative duration", *keepalive)
	}

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		return fmt.Errorf("invalid --log-level %q", *logLevel)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	// Assembly (§1): pathguard canonicalizes/validates the --allow roots at
	// startup (a missing directory is a boot error); the tools.Shared state
	// (Guard + Root pool + Retrier) is process-wide, and every connection —
	// the one stdio connection or each HTTP session — gets its own
	// tools.Conn (session markers and per-path locks) over it.
	shared, err := tools.NewShared(allow, logger)
	if err != nil {
		return err
	}
	logger.Info("starting", "version", version, "transport", *transport, "allow", shared.Guard.AllowedDirs())

	// SIGINT/SIGTERM cancel the context; both transports shut down on it.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *transport == "http" {
		return runHTTP(ctx, shared, logger, *addr, *keepalive, token)
	}
	return runStdio(ctx, shared, logger)
}

// runStdio serves the single stdio connection (§0: default transport, local
// debugging shape — one process per client connection).
func runStdio(ctx context.Context, shared *tools.Shared, logger *slog.Logger) error {
	conn := shared.NewConn()
	srv := mcp.NewServer(&mcp.Implementation{Name: serverName, Version: version}, &mcp.ServerOptions{
		Instructions: serverInstructions(shared),
		Logger:       logger,
	})
	conn.Register(srv)

	err := srv.Run(ctx, &mcp.StdioTransport{})
	if isNormalClose(err) {
		// The client ended the session (stdin EOF / SDK close sentinel).
		// That is a normal shutdown for a stdio server: log it and exit 0
		// so wrapper layers do not misreport it as a crash.
		logger.Info("session closed", "reason", err)
		return nil
	}
	return err
}

// runHTTP serves the resident streamable-HTTP form (§0): MCP endpoint at
// /{token}/mcp (token-path authentication — the token in the path IS the
// credential; the old nginx basic-auth fronting is gone), one *mcp.Server
// (with its own tools.Conn) per StreamableHTTP session, plus the automount
// keepalive loop. Requests with a wrong path die in the mux 404 and never
// reach the SDK handler, so the connection open/close log lines remain the
// audit surface for accepted peers.
func runHTTP(ctx context.Context, shared *tools.Shared, logger *slog.Logger, addr string, keepalive time.Duration, token string) error {
	// §0 keepalive: periodically stat every allowed directory so the CIFS
	// automount (idle timeout 600s) never unmounts the share under us. It
	// dies with ctx on shutdown.
	shared.Keepalive(ctx, keepalive, nil)

	srv := &http.Server{
		Addr:    addr,
		Handler: newMCPHandler(shared, logger, token),
		// Sane timeouts without breaking streamable HTTP: Read timeouts
		// bound slow clients on request reads (POST bodies are small JSON),
		// IdleTimeout reaps keep-alive connections. WriteTimeout is
		// deliberately omitted: responses are SSE streams that legitimately
		// outlive any fixed write budget.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	// The token must never reach stderr logs: the endpoint is recorded as
	// the literal "<token>" placeholder. Rotation is an env change + restart.
	logger.Info("http listening", "addr", addr, "endpoint", "/<token>/mcp", "auth", "token-path")

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		// Graceful drain: in-flight requests finish, live SSE streams are
		// closed by Shutdown, sessions end. ctx is already canceled (it is
		// the signal context), so drain under a fresh deadline.
		drainCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(drainCtx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
		return nil
	}
}

// newMCPHandler builds the token-path routing (§0): exactly one route —
// "/"+token+"/mcp", a Go 1.22 exact-match pattern (no trailing slash) — to
// the SDK StreamableHTTP handler, wrapped by connLogging (connection
// lifecycle audit) inside versionRewrite (protocol-version guard, see
// statefulProtocolVersions) inside discoverRespond (direct answer to
// single server/discover probes, see discoverRespond). Every other path
// (the old /mcp, wrong tokens, /) falls through to the ServeMux default 404
// and is never seen by any wrapper: a 404 is not a connection event.
//
// The SDK handler does not inspect the URL path at all (verified against the
// go-sdk v1.7.0 source), so POST initialize, the standalone SSE GET stream,
// and DELETE session termination all keep working under the tokened prefix
// unchanged. The GetServer callback runs once per new session (verified
// against the same source: serveStatefulPOST calls getServer only when no
// Mcp-Session-Id arrives), so a fresh *mcp.Server + tools.Conn per
// connection is exactly the §0 "one server instance per HTTP connection"
// isolation.
//
// Precondition: token has passed validateToken (run() guarantees this via
// loadToken, and so must any other caller) — the charset excludes every
// ServeMux pattern metacharacter, so the splice into the pattern below
// cannot be redirected by the token itself.
func newMCPHandler(shared *tools.Shared, logger *slog.Logger, token string) *http.ServeMux {
	// Resolved once: every per-session *mcp.Server below AND the discover
	// responder must send the identical text — the discover answer promises
	// what initialize will say.
	instructions := serverInstructions(shared)
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		srv := mcp.NewServer(&mcp.Implementation{Name: serverName, Version: version}, &mcp.ServerOptions{
			Instructions: instructions,
			Logger:       logger,
		})
		shared.NewConn().Register(srv)
		return srv
	}, &mcp.StreamableHTTPOptions{Logger: logger})
	mux := http.NewServeMux()
	// Chain order: discoverRespond runs first (outside) — single
	// server/discover probes are answered before anything else because the
	// stateful SDK handler has no discover answer at all. Then versionRewrite
	// (protocol-version downgrade, header and _meta in lockstep), connLogging
	// closest to the SDK: it reads Mcp-Session-Id and status codes only, so
	// neither rewrite disturbs its audit semantics, and the SDK handler never
	// observes a version signal a stateful server cannot answer.
	mux.Handle("/"+token+"/mcp", discoverRespond(versionRewrite(connLogging(h, logger)), instructions))
	return mux
}

// mcpProtocolVersionHeader is the MCP-Protocol-Version request header
// (spec 2025-06-18 §2.7): clients stamp the negotiated version on every
// post-initialize request. http.Header.Get/Set canonicalize the key, so the
// casing clients send is irrelevant here.
const mcpProtocolVersionHeader = "MCP-Protocol-Version"

// statefulVersions is the canonical ordered form of the protocol-version
// set below: newest first, as the discover response's supportedVersions must
// be offered (the go-sdk protocol docs have clients pick their best match in
// one pass). A pinned literal on purpose, never map iteration — map order is
// random, and this order is a wire contract. Date-shaped version strings
// happen to sort newest-first lexicographically, so the literal reads as the
// sort of itself.
var statefulVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

// statefulProtocolVersions is the exact set of MCP protocol versions this
// server's stateful HTTP transport speaks — everything the go-sdk v1.7.0
// supports on a stateful StreamableHTTPHandler, i.e. statefulVersions as a
// membership set (derived, so what the server advertises in discover and
// what rewriteProtocolVersion accepts cannot drift apart). The SDK also
// knows "2026-07-28" (the SEP-2567 sessionless protocol), but a stateful
// server must reject any request carrying it with HTTP 400 "protocol
// version ... is only supported on stateless HTTP servers"
// (mcp/streamable.go, serveStatefulPOST), and Stateless=true is not
// negotiable here: the per-session read-before-write markers ARE the safety
// model (§0), and they need one resident session per connection — exactly
// what the sessionless protocol removes. The initialize negotiation already
// caps the session at "2025-11-25" (SDK shared.go negotiatedVersion returns
// 2025-11-25 for anything it does not pass through), so rewriting a
// client's stray 2026-07-28 header down into this set keeps the wire
// consistent with what the InitializeResult told the client. Unknown/future
// versions get the same rewrite: forward tolerance without chasing every
// SDK release.
var statefulProtocolVersions = func() map[string]struct{} {
	m := make(map[string]struct{}, len(statefulVersions))
	for _, v := range statefulVersions {
		m[v] = struct{}{}
	}
	return m
}()

// rewriteProtocolVersion decides what MCP-Protocol-Version value the
// stateful server should act on for an incoming v: an accepted version (or
// empty — pre-handshake requests legitimately omit the header; the SDK
// treats that as "version unknown, may be any initialize") passes through
// verbatim; anything else — "2026-07-28" (stateless-only, hard 400 on a
// stateful server) or an unknown/future version — becomes "2025-11-25",
// the initialize-negotiation cap. Pure on purpose: tests pin the decision
// table without any HTTP.
func rewriteProtocolVersion(v string) string {
	if v == "" {
		return ""
	}
	if _, ok := statefulProtocolVersions[v]; ok {
		return v
	}
	return "2025-11-25"
}

// versionRewrite wraps the MCP endpoint and downgrades every protocol-version
// signal a stateful server cannot serve, keeping the MCP-Protocol-Version
// header and the >= 2026-07-28 per-request `params._meta` version in
// lockstep. Constraint it enforces: this server MUST stay stateful
// (per-session read-before-write markers, §0), and go-sdk v1.7.0 kills a
// modern client two ways — a 2026-07-28 header dies in the stateful-only
// 400, and a header/_meta pair that disagrees dies in -32020 "Mcp-Protocol-
// Version header ... does not match request io.modelcontextprotocol/
// protocolVersion" (HTTP 400), which is exactly what a header-only rewrite
// produces for the ChatGPT connector (it stamps header AND _meta 2026-07-28
// on every request). Two shapes, one rule — an out-of-set version becomes
// the legacy request shape:
//
//   - No _meta version anywhere in the body (all legacy traffic; initialize
//     never carries one): the body passes through byte-identical and only
//     the header is rewritten, rewriteProtocolVersion → "2025-11-25" (the
//     initialize-negotiation cap; unknown/future versions too).
//
//   - Messages carrying params._meta["io.modelcontextprotocol/
//     protocolVersion"] outside the set (single object or batch array; other
//     _meta keys survive): the version key is stripped from every such
//     message and an out-of-set header is REMOVED, not rewritten. Why strip
//     the key instead of setting it to "2025-11-25" (both verified against
//     go-sdk v1.7.0, serveStatefulPOST): a stateful handler 400s ANY
//     non-discover message that merely carries a _meta version — the value
//     is irrelevant ("only supported on stateless HTTP servers") — and a
//     header stamped 2025-11-25 re-arms the SDK's >= 2025-06-18 batch
//     rejection, killing whole batches that the in-band path answers message
//     by message. The stripped request is exactly the legacy pre-handshake
//     shape the SDK already tolerates (absent header = version unknown), and
//     because both signals disappear TOGETHER, the -32020 mismatch class is
//     eliminated rather than patched. Re-marshal happens only when something
//     actually changed; a body that fails to parse as JSON passes through
//     untouched (the SDK produces its own, authentic, parse error).
//
// It sits outside connLogging (unchanged: it audits session opens/closes,
// not versions) and inside the token-path route, so only token-bearing
// traffic is rewritten. Deliberately NOT worked around: a batched POST
// carrying an out-of-set header but NO _meta version still gets the
// header-only rewrite and can be refused with 400 "JSON-RPC batching is not
// supported in 2025-06-18 and later" — that is spec-mandated behavior for
// >= 2025-06-18, not a bug.
func versionRewrite(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Both rules inspect the raw body; MCP POST bodies are small JSON
		// (§0), so buffer once and hand downstream either the identical
		// bytes or the re-marshal — never a consumed reader.
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "failed to read request body", http.StatusBadRequest)
			return
		}
		// Quoted, so the scan matches the JSON object key only — not a
		// mention of the field name inside some tool argument (which the
		// structural walk below would then correctly ignore).
		if bytes.Contains(body, []byte(`"`+mcp.MetaKeyProtocolVersion+`"`)) {
			if rewritten, changed := stripUnstatefulMetaVersions(body); changed {
				body = rewritten
				r.ContentLength = int64(len(body)) // re-marshal changed the length
				// Header side of the same downgrade: an out-of-set header is
				// removed (not rewritten) so header and _meta reach the SDK
				// on the legacy shape together; a surviving 2025-11-25 header
				// beside a stripped _meta would re-arm the >= 2025-06-18
				// batch rejection and keep header/_meta asymmetries possible.
				if v := r.Header.Get(mcpProtocolVersionHeader); v != "" {
					if _, ok := statefulProtocolVersions[v]; !ok {
						r.Header.Del(mcpProtocolVersionHeader)
					}
				}
			}
		}
		// Byte-identical restore when nothing was rewritten; r.ContentLength
		// already matches the bytes in both arms.
		r.Body = io.NopCloser(bytes.NewReader(body))
		// Header-only rule for everything the body rewrite did not touch
		// (an in-set header here is a no-op pass-through; a header deleted
		// above is skipped by the Get).
		if v := r.Header.Get(mcpProtocolVersionHeader); v != "" {
			if nv := rewriteProtocolVersion(v); nv != v {
				r.Header.Set(mcpProtocolVersionHeader, nv)
			}
		}
		h.ServeHTTP(w, r)
	})
}

// stripUnstatefulMetaVersions removes params._meta["io.modelcontextprotocol/
// protocolVersion"] from every JSON-RPC message in body whose value lies
// outside statefulProtocolVersions, handling both a single message object
// and a batch array (each element that is an object; non-object batch
// elements are left for the SDK to reject). It returns the body to forward
// and whether anything changed: unchanged bodies return (nil, false) so the
// caller keeps the original bytes — same for a body that is not JSON at
// all, keeping the SDK's own parse error authentic.
func stripUnstatefulMetaVersions(body []byte) ([]byte, bool) {
	var doc any
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, false
	}
	var msgs []map[string]any
	switch v := doc.(type) {
	case map[string]any: // single message
		msgs = append(msgs, v)
	case []any: // batch: every element that is a message
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				msgs = append(msgs, m)
			}
		}
	default: // scalar/null: not JSON-RPC; the SDK will say so
		return nil, false
	}
	changed := false
	for _, msg := range msgs {
		params, ok := msg["params"].(map[string]any)
		if !ok {
			continue
		}
		meta, ok := params["_meta"].(map[string]any)
		if !ok {
			continue
		}
		v, ok := meta[mcp.MetaKeyProtocolVersion].(string)
		if !ok {
			continue
		}
		if _, stateful := statefulProtocolVersions[v]; stateful {
			continue
		}
		// Keep any other _meta keys (progress tokens, client identity):
		// only the version claim is a lie this server must not receive.
		delete(meta, mcp.MetaKeyProtocolVersion)
		changed = true
	}
	if !changed {
		return nil, false
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return nil, false // unreachable: re-marshaling decoded JSON
	}
	return out, true
}

// methodDiscover is the >= 2026-07-28 protocol's pre-handshake probe
// (SEP-2575); the go-sdk spells it identically but keeps the constant
// unexported.
const methodDiscover = "server/discover"

// discoverRespond wraps the MCP endpoint, OUTERMOST inside the token route
// (ahead of versionRewrite), and answers single-message server/discover
// POSTs itself instead of letting the SDK try. Why the SDK cannot answer:
// discover traffic stamped 2026-07-28 is stateless-only there (and
// Stateless=true is not negotiable — §0: the per-session read-before-write
// markers ARE the safety model), while older, consistent signals get
// in-band -32601 "method not found" and a header/_meta split gets -32020 —
// the stateful handler has no usable discover response at all. Why
// supportedVersions is exactly statefulVersions, pointedly WITHOUT
// 2026-07-28: per the go-sdk protocol docs, the designed downgrade path is
// that a discover result not offering the client's version makes it fall
// back to the legacy initialize handshake — which this stateful server
// serves well. Advertising 2026-07-28 would be a lie with teeth: it pushes
// the client into sessionless requests this server must refuse. Only
// SINGLE requests are intercepted: a discover inside a batch flows through
// to versionRewrite + the SDK, which answers the discover element in-band
// (-32601) and the rest normally — consuming a batch element we cannot
// split would strand its siblings. Non-POST requests (the SSE GET stream,
// DELETE session termination) and non-discover POSTs pass through
// untouched. The reply is the DiscoverResult wire shape per go-sdk
// protocol.go (supportedVersions + capabilities required, instructions
// optional; the SDK's cacheability fields are omitted — this answer is
// computed, there is nothing to cache), echoing the request id and sending
// the same instructions text initialize will send, framed per the client's
// Accept: one SSE "message" frame when it lists text/event-stream,
// otherwise raw JSON.
func discoverRespond(h http.Handler, instructions string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "failed to read request body", http.StatusBadRequest)
				return
			}
			// Byte-identical restore for the pass-through path (the
			// intercept path never reaches the inner handler); the length is
			// unchanged, so r.ContentLength stays as the client sent it.
			r.Body = io.NopCloser(bytes.NewReader(body))
			if id, ok := singleDiscoverRequest(body); ok {
				writeDiscoverResult(w, r.Header.Get("Accept"), id, instructions)
				return
			}
		}
		h.ServeHTTP(w, r)
	})
}

// singleDiscoverRequest reports whether body is ONE JSON-RPC request (an
// object, not a batch array) for server/discover, returning its id for the
// response echo. Batches — including a discover among other messages — and
// id-less notifications are not intercepts: the former must keep their
// siblings (see discoverRespond), the latter has nothing to echo.
func singleDiscoverRequest(body []byte) (json.RawMessage, bool) {
	var msg struct {
		ID     *json.RawMessage `json:"id"` // pointer: absent and null both stay nil
		Method string           `json:"method"`
	}
	if err := json.Unmarshal(body, &msg); err != nil {
		return nil, false // batch / malformed / trailing garbage: not ours
	}
	if msg.Method != methodDiscover || msg.ID == nil {
		return nil, false
	}
	return *msg.ID, true
}

// discoverCapabilities is the capabilities object of the discover answer:
// tools are all this server has, and there is no listChanged behavior to
// promise — `{"tools":{}}`, matching what the SDK advertises on initialize.
type discoverCapabilities struct {
	Tools map[string]any `json:"tools"`
}

// discoverResult is the result object of the discover answer (go-sdk
// protocol.go DiscoverResult: supportedVersions + capabilities required,
// instructions optional — cacheability fields omitted, see discoverRespond).
type discoverResult struct {
	SupportedVersions []string             `json:"supportedVersions"`
	Capabilities      discoverCapabilities `json:"capabilities"`
	Instructions      string               `json:"instructions"`
}

// discoverResponse is the full JSON-RPC response envelope for the discover
// answer; the id is echoed as RawMessage so integer ids stay integers and
// string ids stay strings.
type discoverResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  discoverResult  `json:"result"`
}

// writeDiscoverResult emits the discover answer with content negotiation:
// one SSE frame when the client's Accept lists text/event-stream (the
// streamable-HTTP default dual Accept), raw JSON otherwise. A substring
// test on Accept deliberately: it tolerates parameters (";q=0.9") the way
// real Accept headers carry them.
func writeDiscoverResult(w http.ResponseWriter, accept string, id json.RawMessage, instructions string) {
	payload, err := json.Marshal(discoverResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result: discoverResult{
			SupportedVersions: statefulVersions,
			Capabilities:      discoverCapabilities{Tools: map[string]any{}},
			Instructions:      instructions,
		},
	})
	if err != nil {
		http.Error(w, "failed to encode discover result", http.StatusInternalServerError)
		return
	}
	if strings.Contains(accept, "text/event-stream") {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: message\ndata: %s\n\n", payload)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(payload)
}

// validateToken reports whether token fits tokenRE. Pure on purpose: run()
// calls it (via loadToken) before any mux pattern is constructed, and tests
// exercise the charset contract — including the properties that matter for
// routing (no "/", "{", "}") — directly.
func validateToken(token string) error {
	if !tokenRE.MatchString(token) {
		return fmt.Errorf("invalid %s: must be 1-128 characters from [A-Za-z0-9_-] and start with an alphanumeric (URL-path-segment safe)", tokenEnv)
	}
	return nil
}

// loadToken resolves the HTTP transport token from tokenEnv. Unset or empty
// is a startup error naming the variable and the token-path model (the token
// is the credential — there is no second factor); any value then has to pass
// validateToken before the caller may build a route from it.
func loadToken() (string, error) {
	token := os.Getenv(tokenEnv)
	if token == "" {
		return "", fmt.Errorf("%s is required with --transport http: it holds the token that enables token-path authentication (the MCP endpoint is /<token>/mcp)", tokenEnv)
	}
	if err := validateToken(token); err != nil {
		return "", err
	}
	return token, nil
}

// defaultInstructionsFmt is the default initialize-result instructions text
// (MCP's AGENTS.md equivalent): one sentence naming the concrete allowed
// roots — %s receives the comma-joined allowed dirs, as today. It stays this
// short on purpose: the text is injected into the model's context at the
// start of every session, so restating anything the tool JSON schemas
// already carry (per-tool semantics, argument names, the read-first rule) is
// pure token cost and drifts the moment a tool description changes — those
// facts live ONLY in the tool descriptions (internal/tools/stub.go). The
// one thing schemas cannot know is which root paths this deployment actually
// allows: the descriptions reference "the allowed roots configured via
// --allow" without listing them, and this sentence closes that gap.
// Deployment-specific context about the objects being served belongs in the
// FILE_EDIT_MCP_INSTRUCTIONS override, not in this default.
const defaultInstructionsFmt = `All tools operate on files under: %s.`

// serverInstructions resolves the instructions every server construction
// sends in the initialize result (both transports): instructionsEnv wins
// verbatim when non-empty (operator override); otherwise the default
// interpolated with the actual allowed dirs, so the model is told what it
// operates on without any per-deployment build step.
func serverInstructions(shared *tools.Shared) string {
	if s := os.Getenv(instructionsEnv); s != "" {
		return s
	}
	return fmt.Sprintf(defaultInstructionsFmt, strings.Join(shared.Guard.AllowedDirs(), ", "))
}

// connLogging wraps the MCP endpoint and logs connection lifecycle events:
// a POST that creates a session (the SDK stamps the new Mcp-Session-Id onto
// the initialize response) is an open, a DELETE (the client-initiated
// session termination) is a close. It sits behind the token-path route, so
// only requests that already carry the right token reach it: these lines are
// the audit surface for who is actually using a valid token, from where.
func connLogging(h http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		h.ServeHTTP(rec, r)
		switch r.Method {
		case http.MethodPost:
			if id := w.Header().Get("Mcp-Session-Id"); id != "" && r.Header.Get("Mcp-Session-Id") == "" {
				logger.Info("mcp connection opened", "session", id, "remote", r.RemoteAddr)
			}
		case http.MethodDelete:
			if id := r.Header.Get("Mcp-Session-Id"); id != "" && rec.status < 400 {
				logger.Info("mcp connection closed", "session", id, "remote", r.RemoteAddr)
			}
		}
	})
}

// statusRecorder captures the response status so connLogging can log only
// successful session terminations. Unwrap exposes the underlying writer so
// http.NewResponseController (used by the SDK to flush SSE streams) keeps
// working through the wrapper — without it the standalone SSE GET stream
// never flushes and clients hang on Connect.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Unwrap implements the http.ResponseController unwrap protocol.
func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

// isNormalClose reports whether err represents the client ending the stdio
// session rather than a server failure. Two shapes occur in the go-sdk
// (v1.7.0, verified against its source and with an IOTransport harness):
//
//   - io.EOF surfacing through the error chain (matched with errors.Is);
//   - the SDK's internal jsonrpc2.ErrServerClosing — WireError code -32004,
//     Message "server is closing" — wrapped around the read error, e.g.
//     "server is closing: EOF". The sentinel lives in an internal package,
//     unreachable from module code, and its Is matches only by code against
//     another *WireError, so this arm matches the stable message text.
//
// Clean shutdowns return nil from Run and never reach here.
func isNormalClose(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) {
		return true
	}
	return strings.Contains(err.Error(), "server is closing")
}

// allowFlags collects repeated --allow values.
type allowFlags []string

func (a *allowFlags) String() string {
	return strings.Join(*a, ",")
}

func (a *allowFlags) Set(v string) error {
	*a = append(*a, v)
	return nil
}
