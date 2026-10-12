// Restricted HTTPS client (ARCHITECTURE §5.9): the fetch entry (Get), the
// per-stage budgets, the dedicated transport that resolves once per
// connection and dials validated IP literals only, and the error taxonomy
// the tools layer maps onto errmsg rows.

package imagedownload

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"time"
)

// Error taxonomy (§5.9). Kind is one of the sentinels below; Detail is a
// fixed-vocabulary stage or reason string (or a host name / status code
// rendered by the caller) — never a URL, query, header value, or remote
// body. errors.Is(err, ErrXxx) works through wrappers via the Is method.
var (
	ErrBadURL    = errors.New("bad-url")
	ErrBlocked   = errors.New("blocked")
	ErrRedirects = errors.New("redirect-limit")
	ErrStatus    = errors.New("http-status")
	ErrNet       = errors.New("network")
	ErrTimeout   = errors.New("timeout")
	ErrTooLarge  = errors.New("too-large")
	ErrType      = errors.New("image-type")
)

// Stage / reason vocabulary for Detail (model-visible through errmsg
// templates; each string is a complete short phrase).
const (
	StageDNS       = "dns lookup"
	StageConnect   = "connection"
	StageTLS       = "tls handshake"
	StageHeader    = "response header"
	StageBody      = "response body"
	ReasonEncoding = "unsupported content encoding"
	ReasonTrunc    = "truncated response body"
	ReasonConflict = "content type conflicts with the detected image type"
	ReasonEmpty    = "empty response body"
	ReasonRemoteIP = "dialed address changed unexpectedly"
)

// Error is the concrete failure type this package returns.
type Error struct {
	// Kind is the classification sentinel.
	Kind error
	// Detail is the fixed-vocabulary detail (stage, reason, or host).
	Detail string
	// Status carries the HTTP status for ErrStatus.
	Status int
}

func (e *Error) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("%v: HTTP %d", e.Kind, e.Status)
	}
	if e.Detail != "" {
		return fmt.Sprintf("%v: %s", e.Kind, e.Detail)
	}
	return e.Kind.Error()
}

// Is makes errors.Is(err, ErrXxx) match the Kind sentinel.
func (e *Error) Is(target error) bool { return target == e.Kind }

// Limits centralizes the §5.9 download budget. Production uses
// DefaultLimits; the tools layer carries a copy so tests can shrink the
// waits. These are resource ceilings, not completion guarantees.
type Limits struct {
	// MaxURLBytes bounds one URL string (8192).
	MaxURLBytes int
	// MaxImageBytes is the per-image byte cap (20 MiB).
	MaxImageBytes int64
	// MaxRedirects bounds the explicit redirect chain (3).
	MaxRedirects int
	// DNSLimit, ConnectLimit, TLSLimit, HeaderLimit are the per-stage
	// ceilings (2s/3s/3s/5s); each is also bounded by the caller's item
	// deadline.
	DNSLimit, ConnectLimit, TLSLimit, HeaderLimit time.Duration
	// MaxHeaderBytes bounds the response header block (64 KiB).
	MaxHeaderBytes int64
	// SniffBytes is the magic-sniff buffer (512).
	SniffBytes int
	// SVGPrologBytes bounds how far into the body the SVG root element may
	// sit before the file is rejected (64 KiB) — it also bounds the XML
	// token memory of the check.
	SVGPrologBytes int64
	// CopyBuffer is the per-worker copy buffer (32 KiB).
	CopyBuffer int
}

// DefaultLimits returns the §5.9 first-version budget.
func DefaultLimits() Limits {
	return Limits{
		MaxURLBytes:    8192,
		MaxImageBytes:  20 << 20,
		MaxRedirects:   3,
		DNSLimit:       2 * time.Second,
		ConnectLimit:   3 * time.Second,
		TLSLimit:       3 * time.Second,
		HeaderLimit:    5 * time.Second,
		MaxHeaderBytes: 64 << 10,
		SniffBytes:     512,
		SVGPrologBytes: 64 << 10,
		CopyBuffer:     32 << 10,
	}
}

// Response is one accepted (HTTP 200) download ready for streaming. The
// caller owns Body and must Close it.
type Response struct {
	StatusCode int
	Header     http.Header
	Body       io.ReadCloser
	// ContentLength mirrors the declared length (-1 unknown), for the
	// truncation check in ConsumeBody.
	ContentLength int64
	// URL is the final URL after redirects (logging only; never echoed to
	// the model).
	URL *url.URL
}

// Fetcher executes restricted HTTPS GETs. Build with NewFetcher (the
// production wiring); tests override RoundTripper/Policy.Resolver directly
// on the struct. A Fetcher is safe for concurrent use.
type Fetcher struct {
	Policy Policy
	Limits Limits
	// UserAgent is sent verbatim on every request (a fixed browser-style
	// string in production; no other identifying headers are sent).
	UserAgent string
	// RoundTripper replaces the wire execution for tests (fake responses,
	// no dialing). Nil uses the built-in restricted transport.
	RoundTripper http.RoundTripper

	transport *http.Transport
}

// NewFetcher returns the production fetcher: the SSRF policy, the default
// limits, and the dedicated transport described in §5.9. ua is the fixed
// User-Agent every request carries.
func NewFetcher(ua string) *Fetcher {
	f := &Fetcher{Policy: Policy{}, Limits: DefaultLimits(), UserAgent: ua}
	return f
}

// Get validates rawURL, follows at most Limits.MaxRedirects hops (full
// policy re-validation per hop), and returns the accepted 200 response
// after the cheap header-level checks (content-length ceiling, identity
// encoding). Everything downstream of the headers — byte cap, image
// validation — is ConsumeBody's job.
func (f *Fetcher) Get(ctx context.Context, rawURL string) (*Response, error) {
	if len(rawURL) > f.Limits.MaxURLBytes {
		return nil, &Error{Kind: ErrBadURL, Detail: "the URL exceeds the length limit"}
	}
	for i := 0; i < len(rawURL); i++ {
		if rawURL[i] < 0x20 || rawURL[i] == 0x7f {
			return nil, &Error{Kind: ErrBadURL, Detail: "the URL contains control characters"}
		}
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, &Error{Kind: ErrBadURL, Detail: "the URL is not parseable"}
	}
	if err := f.Policy.ValidateURL(u); err != nil {
		return nil, err
	}

	rt := f.roundTripper()
	current := u
	for hop := 0; ; hop++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, current.String(), nil)
		if err != nil {
			return nil, &Error{Kind: ErrBadURL, Detail: "the URL is not usable as a request"}
		}
		req.Header.Set("User-Agent", f.UserAgent)
		req.Header.Set("Accept-Encoding", "identity")
		resp, err := rt.RoundTrip(req)
		if err != nil {
			return nil, classifyTransport(err)
		}
		switch {
		case resp.StatusCode == http.StatusOK:
			if resp.ContentLength > f.Limits.MaxImageBytes {
				drainClose(resp.Body)
				return nil, &Error{Kind: ErrTooLarge}
			}
			if enc := resp.Header.Get("Content-Encoding"); enc != "" && enc != "identity" {
				drainClose(resp.Body)
				return nil, &Error{Kind: ErrNet, Detail: ReasonEncoding}
			}
			return &Response{
				StatusCode:    resp.StatusCode,
				Header:        resp.Header,
				Body:          resp.Body,
				ContentLength: resp.ContentLength,
				URL:           current,
			}, nil
		case isRedirect(resp.StatusCode):
			drainClose(resp.Body)
			loc := resp.Header.Get("Location")
			if loc == "" {
				return nil, &Error{Kind: ErrStatus, Status: resp.StatusCode}
			}
			if hop >= f.Limits.MaxRedirects {
				return nil, &Error{Kind: ErrRedirects}
			}
			locURL, err := url.Parse(loc)
			if err != nil {
				return nil, &Error{Kind: ErrBadURL, Detail: "the redirect location is not parseable"}
			}
			next := current.ResolveReference(locURL)
			if err := f.Policy.ValidateURL(next); err != nil {
				return nil, err
			}
			current = next
		default:
			drainClose(resp.Body)
			return nil, &Error{Kind: ErrStatus, Status: resp.StatusCode}
		}
	}
}

// isRedirect reports the hop statuses handled by the explicit loop.
func isRedirect(code int) bool {
	switch code {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	}
	return false
}

// drainClose releases a rejected response body without unbounded reads.
func drainClose(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 4096))
	_ = body.Close()
}

// roundTripper returns the injected test transport or the restricted one
// (built once; the transport pools connections per origin).
func (f *Fetcher) roundTripper() http.RoundTripper {
	if f.RoundTripper != nil {
		return f.RoundTripper
	}
	if f.transport == nil {
		f.transport = &http.Transport{
			// §5.9: no environment proxy, no caller headers, no HTTP/2
			// (HTTP/1.1 only via the dial-side NextProtos), identity
			// encoding only, header-block ceiling, response-header budget.
			Proxy:                  nil,
			DialTLSContext:         f.dialTLS,
			ForceAttemptHTTP2:      false,
			DisableCompression:     true,
			MaxIdleConns:           8,
			MaxIdleConnsPerHost:    4,
			IdleConnTimeout:        60 * time.Second,
			ResponseHeaderTimeout:  f.Limits.HeaderLimit,
			MaxResponseHeaderBytes: f.Limits.MaxHeaderBytes,
		}
	}
	return f.transport
}

// dialTLS is the security core (§5.9 step 5): resolve the origin once per
// NEW connection through the policy, dial one of the validated IP
// literals (fallback only within the validated list), verify the connected
// address, then TLS-handshake for the ORIGINAL host name (SNI +
// certificate verification against the name, never the IP). No code path
// re-resolves the name after validation — the transport only ever hands
// this function URL hosts.
func (f *Fetcher) dialTLS(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, &Error{Kind: ErrBadURL, Detail: reasonEmptyHost}
	}
	if port != "443" {
		return nil, &Error{Kind: ErrBadURL, Detail: reasonPort}
	}

	ips, err := f.resolveOnce(ctx, host)
	if err != nil {
		return nil, err
	}

	var lastErr error
	for _, ip := range ips {
		conn, err := f.dialOne(ctx, host, ip)
		if err != nil {
			lastErr = err
			continue
		}
		return conn, nil
	}
	return nil, lastErr
}

// resolveOnce runs the DNS stage under its own budget (bounded by ctx's
// remaining deadline) and applies the address verdict to the whole answer
// set.
func (f *Fetcher) resolveOnce(ctx context.Context, host string) ([]netip.Addr, error) {
	rctx, cancel := stageCtx(ctx, f.Limits.DNSLimit)
	defer cancel()
	ips, err := f.Policy.ResolveHost(rctx, host)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || rctx.Err() != nil {
			return nil, &Error{Kind: ErrTimeout, Detail: StageDNS}
		}
		return nil, err
	}
	return ips, nil
}

// dialOne connects to ip:443 and handshakes TLS for host. The connected
// RemoteAddr is compared against ip before and after the handshake: a
// mismatch (transparent proxy, rebinding middlebox) closes the connection.
func (f *Fetcher) dialOne(ctx context.Context, host string, ip netip.Addr) (net.Conn, error) {
	d := net.Dialer{Timeout: f.Limits.ConnectLimit, KeepAlive: 30 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", netip.AddrPortFrom(ip, 443).String())
	if err != nil {
		return nil, classifyTransport(err)
	}
	if !remoteIs(conn, ip) {
		_ = conn.Close()
		return nil, &Error{Kind: ErrNet, Detail: ReasonRemoteIP}
	}

	tlsConn := tls.Client(conn, &tls.Config{
		ServerName: host,
		NextProtos: []string{"http/1.1"}, // no h2: keeps the audit surface HTTP/1.1
		MinVersion: tls.VersionTLS12,
	})
	hctx, cancel := stageCtx(ctx, f.Limits.TLSLimit)
	defer cancel()
	if deadline, ok := hctx.Deadline(); ok {
		_ = tlsConn.SetDeadline(deadline)
	}
	if err := tlsConn.HandshakeContext(hctx); err != nil {
		_ = tlsConn.Close()
		if errors.Is(err, context.DeadlineExceeded) || hctx.Err() != nil {
			return nil, &Error{Kind: ErrTimeout, Detail: StageTLS}
		}
		return nil, &Error{Kind: ErrNet, Detail: StageTLS}
	}
	_ = tlsConn.SetDeadline(time.Time{})
	if !remoteIs(tlsConn, ip) {
		_ = tlsConn.Close()
		return nil, &Error{Kind: ErrNet, Detail: ReasonRemoteIP}
	}
	return tlsConn, nil
}

// remoteIs reports whether conn's peer is exactly ip.
func remoteIs(conn net.Conn, ip netip.Addr) bool {
	ra, ok := conn.RemoteAddr().(*net.TCPAddr)
	if !ok {
		return false
	}
	rip, ok := netip.AddrFromSlice(ra.IP)
	if !ok {
		return false
	}
	return rip.Unmap() == ip
}

// classifyTransport maps a transport/dial error onto the taxonomy. Errors
// this package already classified pass through; raw network errors become
// ErrNet (context deadlines become ErrTimeout) with their detail NEVER
// taken from the raw error text (it can embed the URL with its query).
func classifyTransport(err error) error {
	var ours *Error
	if errors.As(err, &ours) {
		return ours
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &Error{Kind: ErrTimeout, Detail: StageHeader}
	}
	var nerr net.Error
	if errors.As(err, &nerr) && nerr.Timeout() {
		return &Error{Kind: ErrTimeout, Detail: StageHeader}
	}
	return &Error{Kind: ErrNet, Detail: StageConnect}
}

// stageCtx derives a per-stage context: the stage ceiling, but never more
// than the remaining parent deadline.
func stageCtx(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if deadline, ok := parent.Deadline(); ok {
		if remain := time.Until(deadline); remain < d {
			d = remain
		}
	}
	return context.WithTimeout(parent, d)
}
