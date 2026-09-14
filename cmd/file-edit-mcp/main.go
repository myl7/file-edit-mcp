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
	"context"
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
// the SDK StreamableHTTP handler wrapped by connLogging. Every other path
// (the old /mcp, wrong tokens, /) falls through to the ServeMux default 404
// and is never seen by connLogging: a 404 is not a connection event.
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
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		srv := mcp.NewServer(&mcp.Implementation{Name: serverName, Version: version}, &mcp.ServerOptions{
			Instructions: serverInstructions(shared),
			Logger:       logger,
		})
		shared.NewConn().Register(srv)
		return srv
	}, &mcp.StreamableHTTPOptions{Logger: logger})
	mux := http.NewServeMux()
	mux.Handle("/"+token+"/mcp", connLogging(h, logger))
	return mux
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
