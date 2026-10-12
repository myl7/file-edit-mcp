// Client tests (§5.9) over a fake RoundTripper (no dialing, no network):
// request hygiene, redirect policy, status handling, header-level limits,
// and the body pipeline (byte cap, magic/SVG validation, Content-Type
// cross-check) via ConsumeBody.

package imagedownload

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

// pngBytes is a minimal valid PNG header (signature + IHDR chunk start).
var pngBytes = append([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 13}, []byte("IHDR")...)

// fakeRT serves canned per-path responses.
type fakeRT struct {
	responses map[string]*http.Response
	gotURLs   []string
	gotUA     string
	gotEnc    string
}

func (f *fakeRT) RoundTrip(req *http.Request) (*http.Response, error) {
	f.gotURLs = append(f.gotURLs, req.URL.String())
	f.gotUA = req.Header.Get("User-Agent")
	f.gotEnc = req.Header.Get("Accept-Encoding")
	if resp, ok := f.responses[req.URL.Path]; ok {
		return resp, nil
	}
	return &http.Response{
		StatusCode: 404, Status: "404 Not Found", Header: http.Header{},
		Body: io.NopCloser(strings.NewReader("missing")), Request: req,
	}, nil
}

func rtBody(b []byte) io.ReadCloser { return io.NopCloser(bytes.NewReader(b)) }

func resp(status int, body []byte, hdr http.Header) *http.Response {
	if hdr == nil {
		hdr = http.Header{}
	}
	return &http.Response{
		StatusCode: status, Header: hdr, Body: rtBody(body),
		ContentLength: int64(len(body)),
	}
}

func newTestFetcher(responses map[string]*http.Response) (*Fetcher, *fakeRT) {
	rt := &fakeRT{responses: responses}
	f := NewFetcher("test-agent/1.0")
	f.RoundTripper = rt
	return f, rt
}

func TestGetHappyPathHeaders(t *testing.T) {
	f, rt := newTestFetcher(map[string]*http.Response{
		"/a.png": resp(200, pngBytes, nil),
	})
	r, err := f.Get(context.Background(), "https://example.com/a.png")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer r.Body.Close()
	if rt.gotUA != "test-agent/1.0" {
		t.Errorf("User-Agent = %q, want the fixed agent", rt.gotUA)
	}
	if rt.gotEnc != "identity" {
		t.Errorf("Accept-Encoding = %q, want identity", rt.gotEnc)
	}
	if len(rt.gotURLs) != 1 {
		t.Errorf("requests = %v, want exactly one", rt.gotURLs)
	}
}

func TestGetURLPolicy(t *testing.T) {
	f, _ := newTestFetcher(nil)
	bad := []string{
		"http://example.com/a.png",
		"https://user@example.com/a.png",
		"https://example.com:8443/a.png",
		"https://localhost/a.png",
		"https://example.com/a\x00.png",
		strings.Repeat("https://example.com/", 600),
	}
	for _, u := range bad {
		if _, err := f.Get(context.Background(), u); err == nil {
			t.Errorf("Get(%q) accepted", u)
		}
	}
}

func TestGetStatuses(t *testing.T) {
	for _, status := range []int{404, 429, 500, 206, 304} {
		f, _ := newTestFetcher(map[string]*http.Response{
			"/x": resp(status, []byte("no"), nil),
		})
		_, err := f.Get(context.Background(), "https://example.com/x")
		if err == nil {
			t.Fatalf("HTTP %d accepted", status)
		}
		e, ok := err.(*Error)
		if !ok || e.Kind != ErrStatus || e.Status != status {
			t.Errorf("HTTP %d -> %v, want ErrStatus(%d)", status, err, status)
		}
	}
}

func TestGetRedirects(t *testing.T) {
	hop := http.Header{}
	hop.Set("Location", "/hop2")
	hop2 := http.Header{}
	hop2.Set("Location", "/final.png")
	f, _ := newTestFetcher(map[string]*http.Response{
		"/start":     resp(302, nil, hop),
		"/hop2":      resp(301, nil, hop2),
		"/final.png": resp(200, pngBytes, nil),
	})
	r, err := f.Get(context.Background(), "https://example.com/start")
	if err != nil {
		t.Fatalf("3-hop redirect chain: %v", err)
	}
	_ = r.Body.Close()
	if r.URL.Path != "/final.png" {
		t.Errorf("final URL = %s, want /final.png", r.URL)
	}

	// A fourth hop is refused.
	f2, _ := newTestFetcher(map[string]*http.Response{
		"/s":  resp(302, nil, http.Header{"Location": []string{"/h1"}}),
		"/h1": resp(302, nil, http.Header{"Location": []string{"/h2"}}),
		"/h2": resp(302, nil, http.Header{"Location": []string{"/h3"}}),
		"/h3": resp(302, nil, http.Header{"Location": []string{"/h4"}}),
		"/h4": resp(200, pngBytes, nil),
	})
	if _, err := f2.Get(context.Background(), "https://example.com/s"); err == nil {
		t.Error("4-hop chain accepted")
	} else if e, ok := err.(*Error); !ok || e.Kind != ErrRedirects {
		t.Errorf("4-hop chain -> %v, want ErrRedirects", err)
	}

	// Downgrade and internal redirect targets are refused with the URL
	// policy, not followed.
	f3, _ := newTestFetcher(map[string]*http.Response{
		"/s": resp(302, nil, http.Header{"Location": []string{"http://example.com/final.png"}}),
	})
	if _, err := f3.Get(context.Background(), "https://example.com/s"); err == nil {
		t.Error("https->http redirect accepted")
	}
	f4, _ := newTestFetcher(map[string]*http.Response{
		"/s": resp(302, nil, http.Header{"Location": []string{"http://10.0.0.1/x.png"}}),
	})
	if _, err := f4.Get(context.Background(), "https://example.com/s"); err == nil {
		t.Error("redirect to internal host accepted")
	}
	// 302 without a Location header is a plain status error.
	f5, _ := newTestFetcher(map[string]*http.Response{
		"/s": resp(302, nil, http.Header{}),
	})
	if _, err := f5.Get(context.Background(), "https://example.com/s"); err == nil {
		t.Error("302 without Location accepted")
	}
}

func TestGetHeaderLimits(t *testing.T) {
	// Declared length over the cap: pre-rejected.
	f, _ := newTestFetcher(map[string]*http.Response{
		"/big": {StatusCode: 200, Header: http.Header{}, Body: rtBody(nil), ContentLength: 21 << 20},
	})
	if _, err := f.Get(context.Background(), "https://example.com/big"); err == nil {
		t.Error("over-cap Content-Length accepted")
	} else if e, ok := err.(*Error); !ok || e.Kind != ErrTooLarge {
		t.Errorf("over-cap Content-Length -> %v, want ErrTooLarge", err)
	}

	// Compressed transfer is refused even though we asked for identity.
	gz := resp(200, []byte("x"), nil)
	gz.Header.Set("Content-Encoding", "gzip")
	f2, _ := newTestFetcher(map[string]*http.Response{"/gz": gz})
	if _, err := f2.Get(context.Background(), "https://example.com/gz"); err == nil {
		t.Error("gzip Content-Encoding accepted")
	}
}

func TestConsumeBodyBinary(t *testing.T) {
	f, _ := newTestFetcher(map[string]*http.Response{
		"/a.png": resp(200, pngBytes, nil),
	})
	r, err := f.Get(context.Background(), "https://example.com/a.png")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var sink bytes.Buffer
	n, det, err := f.ConsumeBody(r, &sink)
	if err != nil {
		t.Fatalf("ConsumeBody: %v", err)
	}
	if n != int64(len(pngBytes)) || !bytes.Equal(sink.Bytes(), pngBytes) {
		t.Errorf("sink got %d bytes %q, want exact %d-byte body", n, sink.Bytes(), len(pngBytes))
	}
	if det.MIME != "image/png" || det.Ext != ".png" {
		t.Errorf("detected = %+v, want image/png", det)
	}
}

func TestConsumeBodySVG(t *testing.T) {
	svg := []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg" width="10"><rect width="10" height="10"/></svg>`)
	f, _ := newTestFetcher(map[string]*http.Response{
		"/b.svg": resp(200, svg, nil),
	})
	r, _ := f.Get(context.Background(), "https://example.com/b.svg")
	defer r.Body.Close()
	var sink bytes.Buffer
	n, det, err := f.ConsumeBody(r, &sink)
	if err != nil {
		t.Fatalf("ConsumeBody(svg): %v", err)
	}
	if n != int64(len(svg)) || !bytes.Equal(sink.Bytes(), svg) {
		t.Error("svg bytes not forwarded exactly")
	}
	if det.MIME != "image/svg+xml" {
		t.Errorf("detected = %s, want image/svg+xml", det.MIME)
	}
}

func TestConsumeBodyRejects(t *testing.T) {
	cases := []struct {
		name     string
		body     []byte
		header   http.Header
		wantKind error
	}{
		{"html as svg", []byte("<html><svg xmlns=\"http://www.w3.org/2000/svg\"></html>"), nil, ErrType},
		{"svg without ns", []byte("<svg width='1'></svg>"), nil, ErrType},
		{"svg with dtd", []byte("<?xml version='1.0'?><!DOCTYPE svg [<!ENTITY x 'y'>]><svg xmlns='http://www.w3.org/2000/svg'></svg>"), nil, ErrType},
		{"svg with pi", []byte("<?xml-stylesheet href='x'?><?xml version='1.0'?><svg xmlns='http://www.w3.org/2000/svg'></svg>"), nil, ErrType},
		{"pdf", []byte("%PDF-1.7 ...."), nil, ErrType},
		{"empty", nil, nil, ErrType},
		{"type conflict", pngBytes, http.Header{"Content-Type": []string{"image/jpeg"}}, ErrType},
		{"html content-type", pngBytes, http.Header{"Content-Type": []string{"text/html"}}, ErrType},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := newTestFetcher(map[string]*http.Response{
				"/f": resp(200, tc.body, tc.header),
			})
			r, err := f.Get(context.Background(), "https://example.com/f")
			if err != nil {
				t.Fatal(err)
			}
			defer r.Body.Close()
			_, _, err = f.ConsumeBody(r, io.Discard)
			if err == nil {
				t.Fatal("accepted")
			}
			if e, ok := err.(*Error); !ok || e.Kind != tc.wantKind {
				t.Fatalf("err = %v, want kind %v", err, tc.wantKind)
			}
		})
	}

	// octet-stream and missing Content-Type both accept a valid magic.
	for _, ct := range []string{"", "application/octet-stream"} {
		hdr := http.Header{}
		if ct != "" {
			hdr.Set("Content-Type", ct)
		}
		f, _ := newTestFetcher(map[string]*http.Response{
			"/f": resp(200, pngBytes, hdr),
		})
		r, _ := f.Get(context.Background(), "https://example.com/f")
		if _, det, err := f.ConsumeBody(r, io.Discard); err != nil || det.MIME != "image/png" {
			t.Errorf("Content-Type %q: (%s, %v), want png accepted", ct, det.MIME, err)
		}
		_ = r.Body.Close()
	}
}

func TestConsumeBodyCapAndTruncation(t *testing.T) {
	// Streaming cap (no usable declared length): the capReader must stop
	// the copy. These Responses are built directly because Get's
	// header-level Content-Length pre-check would fire first.
	f := NewFetcher("t")
	f.Limits.MaxImageBytes = int64(len(pngBytes))

	big := &Response{Header: http.Header{}, Body: rtBody(append(append([]byte{}, pngBytes...), 0)), ContentLength: -1}
	if _, _, err := f.ConsumeBody(big, io.Discard); err == nil {
		t.Fatal("over-cap body accepted")
	} else if e, ok := err.(*Error); !ok || e.Kind != ErrTooLarge {
		t.Fatalf("over-cap -> %v, want ErrTooLarge", err)
	}

	fit := &Response{Header: http.Header{}, Body: rtBody(pngBytes), ContentLength: -1}
	if _, _, err := f.ConsumeBody(fit, io.Discard); err != nil {
		t.Errorf("at-cap body rejected: %v", err)
	}

	// Declared length larger than the delivered body is truncation.
	f2 := NewFetcher("t")
	cut := &Response{Header: http.Header{}, Body: rtBody(pngBytes), ContentLength: 1 << 20}
	if _, _, err := f2.ConsumeBody(cut, io.Discard); err == nil {
		t.Fatal("truncated body accepted")
	} else if e, ok := err.(*Error); !ok || e.Kind != ErrNet || e.Detail != ReasonTrunc {
		t.Fatalf("truncated -> %v, want ErrNet/%s", err, ReasonTrunc)
	}
}

// TestProductionTransportShape pins the §5.9 transport posture: no proxy,
// no auto HTTP/2, no auto compression.
func TestProductionTransportShape(t *testing.T) {
	f := NewFetcher("agent")
	rt := f.roundTripper()
	tr, ok := rt.(*http.Transport)
	if !ok {
		t.Fatalf("production roundtripper is %T, want *http.Transport", rt)
	}
	if tr.Proxy != nil {
		t.Error("transport has a Proxy function")
	}
	if tr.ForceAttemptHTTP2 {
		t.Error("transport forces HTTP/2")
	}
	if !tr.DisableCompression {
		t.Error("transport auto-compression enabled")
	}
	if tr.DialTLSContext == nil {
		t.Error("transport has no policy dialer")
	}
}
