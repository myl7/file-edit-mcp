// upload tool tests (§5.9/§9): the whole download→stream→compare/publish
// pipeline over a fake RoundTripper (never any network), the result
// aggregation contract (any-failed ⇒ IsError, full results always), the
// degraded no-replace mode, and the freshness interplay with the text
// tools.

package tools

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/myl7/file-edit-mcp/internal/errmsg"
	"github.com/myl7/file-edit-mcp/internal/imagedownload"
)

// pngBytes: signature + IHDR — a minimal valid PNG head.
var upPNG = append([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 13}, []byte("IHDR")...)

var upSVG = []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg" width="4" height="4"><rect width="4" height="4"/></svg>`)

// rtFetch is the fake wire: canned response builders per URL path (a
// consumed io.ReadCloser cannot be reused, so every trip builds a fresh
// body), plus an onTrip hook run WITHOUT the mutex held (so it may block
// to gate concurrency tests).
type rtFetch struct {
	mu        sync.Mutex
	responses map[string]func() *http.Response
	onTrip    func()
	trips     int
}

func (f *rtFetch) RoundTrip(req *http.Request) (*http.Response, error) {
	if f.onTrip != nil {
		f.onTrip()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.trips++
	if build, ok := f.responses[req.URL.Path]; ok {
		return build(), nil
	}
	return &http.Response{
		StatusCode: 404, Status: "404 Not Found", Header: http.Header{},
		Body: io.NopCloser(strings.NewReader("missing")), Request: req,
	}, nil
}

func (f *rtFetch) tripsNow() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.trips
}

// rtResp returns a fresh-body response builder.
func rtResp(status int, body []byte, hdr http.Header) func() *http.Response {
	return func() *http.Response {
		h := http.Header{}
		for k, v := range hdr {
			h[k] = v
		}
		return &http.Response{
			StatusCode: status, Header: h,
			Body:          io.NopCloser(bytes.NewReader(body)),
			ContentLength: int64(len(body)),
		}
	}
}

// startUploadServer boots the standard in-memory server with Shared.Fetch
// swapped for a real imagedownload fetcher over the fake wire.
func startUploadServer(t *testing.T) (*mcpClientSession, *Conn, string, *rtFetch) {
	t.Helper()
	cs, conn, dir := startTestServer(t)
	rt := &rtFetch{responses: map[string]func() *http.Response{}}
	f := imagedownload.NewFetcher("upload-test/1.0")
	f.RoundTripper = rt
	conn.Fetch = f
	return cs, conn, dir, rt
}

// uploadCall invokes upload and decodes the output plus IsError.
func uploadCall(t *testing.T, cs *mcpClientSession, files []any) (UploadOutput, bool) {
	t.Helper()
	text, isErr := callTool(t, cs, "upload", map[string]any{"files": files})
	var out UploadOutput
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("upload result is not UploadOutput JSON: %v (%s)", err, text)
	}
	return out, isErr
}

func item(url, path string) map[string]any {
	return map[string]any{"url": "https://cdn.example.com" + url, "file_path": path}
}

func TestUploadCreated(t *testing.T) {
	cs, _, dir, rt := startUploadServer(t)
	rt.responses["/a.png"] = rtResp(200, upPNG, nil)
	target := filepath.Join(dir, "a.png")

	out, isErr := uploadCall(t, cs, []any{item("/a.png", target)})
	if isErr {
		t.Fatal("upload call errored")
	}
	if out.Summary != (UploadSummary{Created: 1}) {
		t.Fatalf("summary = %+v, want 1 created", out.Summary)
	}
	r := out.Results[0]
	if r.Status != "created" || r.Index != 1 || r.FilePath != target {
		t.Fatalf("result = %+v", r)
	}
	if r.Bytes != int64(len(upPNG)) {
		t.Errorf("bytes = %d, want %d", r.Bytes, len(upPNG))
	}
	sum := sha256.Sum256(upPNG)
	if r.SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("sha256 = %s, want %s", r.SHA256, hex.EncodeToString(sum[:]))
	}
	if r.ContentType != "image/png" {
		t.Errorf("content_type = %s", r.ContentType)
	}
	got, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(got, upPNG) {
		t.Fatalf("disk bytes differ: %v", err)
	}
	if info, err := os.Stat(target); err != nil || info.Mode().Perm() != 0o644 {
		t.Errorf("perm = %v (%v), want 0644", info.Mode().Perm(), err)
	}
	if residue := tempResidue(t, dir); len(residue) != 0 {
		t.Errorf("temp residue after success: %v", residue)
	}
}

func TestUploadSkippedSameContent(t *testing.T) {
	cs, _, dir, rt := startUploadServer(t)
	rt.responses["/a.png"] = rtResp(200, upPNG, nil)
	target := filepath.Join(dir, "a.png")

	if _, isErr := uploadCall(t, cs, []any{item("/a.png", target)}); isErr {
		t.Fatal("first upload failed")
	}
	before, _ := os.Stat(target)
	out, isErr := uploadCall(t, cs, []any{item("/a.png", target)})
	if isErr {
		t.Fatalf("re-upload errored: %+v", out)
	}
	if out.Results[0].Status != "skipped" || out.Summary.Skipped != 1 {
		t.Fatalf("re-upload = %+v, want skipped", out)
	}
	after, _ := os.Stat(target)
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("skipped re-upload touched the target's mtime")
	}
}

func TestUploadExistsConflict(t *testing.T) {
	cs, _, dir, rt := startUploadServer(t)
	rt.responses["/a.png"] = rtResp(200, upPNG, nil)
	target := filepath.Join(dir, "a.png")
	writeDisk(t, target, "different bytes entirely", 0o644)

	out, isErr := uploadCall(t, cs, []any{item("/a.png", target)})
	if !isErr {
		t.Fatal("conflicting upload did not error the call")
	}
	if out.Results[0].Status != "failed" || out.Results[0].ErrorCode != "EExistsConflict" {
		t.Fatalf("result = %+v, want failed/EExistsConflict", out.Results[0])
	}
	if got, _ := os.ReadFile(target); string(got) != "different bytes entirely" {
		t.Error("existing target was modified")
	}
	if residue := tempResidue(t, dir); len(residue) != 0 {
		t.Errorf("temp residue after conflict: %v", residue)
	}
}

func TestUploadExtensionMismatch(t *testing.T) {
	cs, _, dir, rt := startUploadServer(t)
	rt.responses["/pic"] = rtResp(200, upPNG, nil)

	out, isErr := uploadCall(t, cs, []any{item("/pic", filepath.Join(dir, "pic.svg"))})
	if !isErr {
		t.Fatal("png-into-.svg did not error")
	}
	r := out.Results[0]
	if r.Status != "failed" || r.ErrorCode != "EImageType" {
		t.Fatalf("result = %+v, want failed/EImageType", r)
	}
	if r.DetectedContentType != "image/png" || r.SuggestedExtension != ".png" {
		t.Errorf("mismatch detail = %s %s", r.DetectedContentType, r.SuggestedExtension)
	}
	if _, err := os.Stat(filepath.Join(dir, "pic.svg")); !os.IsNotExist(err) {
		t.Error("mismatch created the target anyway")
	}
}

func TestUploadUnsupportedExtensionPreflight(t *testing.T) {
	cs, _, dir, rt := startUploadServer(t)
	rt.responses["/a.png"] = rtResp(200, upPNG, nil)

	out, isErr := uploadCall(t, cs, []any{item("/a.png", filepath.Join(dir, "a.txt"))})
	if !isErr || out.Results[0].ErrorCode != "EImageType" {
		t.Fatalf("result = %+v isErr=%v, want failed/EImageType", out.Results[0], isErr)
	}
	if rt.tripsNow() != 0 {
		t.Error("unsupported extension fetched before failing")
	}
}

func TestUploadURLPolicyAndStatus(t *testing.T) {
	cs, _, dir, _ := startUploadServer(t)

	cases := []struct {
		url       string
		wantCode  string
		wantError string
	}{
		{"http://cdn.example.com/a.png", "EInvalidURL", "https"},
		{"https://user@cdn.example.com/a.png", "EInvalidURL", "credentials"},
		{"https://cdn.example.com:8443/a.png", "EInvalidURL", "port"},
		{"https://localhost/a.png", "EInvalidURL", "single-label"},
		{"https://cdn.example.com/missing.png", "EHTTPStatus", "HTTP 404"},
	}
	var lastJSON []byte
	for _, tc := range cases {
		out, isErr := uploadCall(t, cs, []any{
			map[string]any{"url": tc.url, "file_path": filepath.Join(dir, "x.png")},
		})
		if !isErr {
			t.Errorf("%s: call not errored", tc.url)
			continue
		}
		r := out.Results[0]
		if r.Status != "failed" || r.ErrorCode != tc.wantCode || !strings.Contains(r.Error, tc.wantError) {
			t.Errorf("%s: result = %+v, want failed/%s containing %q", tc.url, r, tc.wantCode, tc.wantError)
		}
		lastJSON, _ = json.Marshal(out)
	}
	// The result never echoes the URL.
	if bytes.Contains(lastJSON, []byte("cdn.example.com")) {
		t.Error("result JSON echoed the source URL")
	}
}

func TestUploadDuplicates(t *testing.T) {
	cs, _, dir, rt := startUploadServer(t)
	rt.responses["/a.png"] = rtResp(200, upPNG, nil)
	target := filepath.Join(dir, "a.png")

	out, isErr := uploadCall(t, cs, []any{
		item("/a.png", target), item("/a.png", target),
	})
	if !isErr || out.Summary.Failed != 2 {
		t.Fatalf("summary = %+v isErr=%v, want 2 failed", out.Summary, isErr)
	}
	for _, r := range out.Results {
		if r.ErrorCode != "EDuplicateTarget" {
			t.Errorf("result %+v, want EDuplicateTarget", r)
		}
	}
	if rt.tripsNow() != 0 {
		t.Error("duplicate items fetched before failing")
	}
}

func TestUploadBatchShape(t *testing.T) {
	cs, _, dir, _ := startUploadServer(t)

	// Empty batch.
	text, isErr := callTool(t, cs, "upload", map[string]any{"files": []any{}})
	if !isErr || !strings.Contains(text, "invalid upload batch") {
		t.Errorf("empty batch = (%q, %v)", text, isErr)
	}
	// Over the cap: a call-level shape error, before any item runs.
	var many []any
	for i := 0; i < 33; i++ {
		many = append(many, item("/a.png", filepath.Join(dir, "a.png")))
	}
	if text, isErr = callTool(t, cs, "upload", map[string]any{"files": many}); !isErr ||
		!strings.Contains(text, "at most 32") {
		t.Errorf("33-item batch = (%q, %v), want call-level shape error", text, isErr)
	}
	// Missing fields are call-level shape errors.
	if text, isErr = callTool(t, cs, "upload", map[string]any{"files": []any{
		map[string]any{"url": "", "file_path": "/x"},
	}}); !isErr || !strings.Contains(text, "url is empty") {
		t.Errorf("empty url = (%q, %v)", text, isErr)
	}
	if text, isErr = callTool(t, cs, "upload", map[string]any{"files": []any{
		map[string]any{"url": "https://cdn.example.com/a.png", "file_path": ""},
	}}); !isErr || !strings.Contains(text, "file_path is empty") {
		t.Errorf("empty file_path = (%q, %v)", text, isErr)
	}
}

func TestUploadPartialFailureIsError(t *testing.T) {
	cs, _, dir, rt := startUploadServer(t)
	rt.responses["/ok.png"] = rtResp(200, upPNG, nil)

	out, isErr := uploadCall(t, cs, []any{
		item("/ok.png", filepath.Join(dir, "ok.png")),
		item("/gone.png", filepath.Join(dir, "gone.png")),
	})
	if !isErr {
		t.Fatal("partial failure did not set IsError")
	}
	if out.Summary != (UploadSummary{Created: 1, Failed: 1}) {
		t.Fatalf("summary = %+v", out.Summary)
	}
	if out.Results[0].Status != "created" || out.Results[1].Status != "failed" {
		t.Fatalf("results = %+v", out.Results)
	}
	if out.Results[1].Index != 2 {
		t.Errorf("failed item index = %d, want 2 (input order)", out.Results[1].Index)
	}
	if len(out.Results) != 2 {
		t.Errorf("results len = %d, want every input answered", len(out.Results))
	}
}

func TestUploadDegradedMode(t *testing.T) {
	cs, conn, dir, rt := startUploadServer(t)
	rt.responses["/a.png"] = rtResp(200, upPNG, nil)
	conn.link = func(r *os.Root, oldname, newname string) error {
		return syscall.EOPNOTSUPP
	}
	target := filepath.Join(dir, "a.png")

	out, isErr := uploadCall(t, cs, []any{item("/a.png", target)})
	if isErr {
		t.Fatalf("degraded upload failed: %+v", out)
	}
	r := out.Results[0]
	if r.Status != "created" {
		t.Fatalf("status = %s", r.Status)
	}
	if !strings.Contains(r.WarningCode, errmsg.CodeAtomicDegraded) {
		t.Errorf("warning_code = %q, want %s", r.WarningCode, errmsg.CodeAtomicDegraded)
	}
	got, _ := os.ReadFile(target)
	if !bytes.Equal(got, upPNG) {
		t.Error("degraded publish wrote different bytes")
	}
	if residue := tempResidue(t, dir); len(residue) != 0 {
		t.Errorf("temp residue after degraded publish: %v", residue)
	}
}

func TestUploadLinkRaceFallsBackToCompare(t *testing.T) {
	cs, conn, dir, rt := startUploadServer(t)
	rt.responses["/a.png"] = rtResp(200, upPNG, nil)
	target := filepath.Join(dir, "a.png")
	// Someone creates the identical file between our missing-check and
	// Link; the lying Link reports EEXIST and the compare branch must
	// converge to skipped.
	writeDisk(t, target, string(upPNG), 0o644)
	conn.link = func(r *os.Root, oldname, newname string) error {
		return syscall.EEXIST
	}

	out, isErr := uploadCall(t, cs, []any{item("/a.png", target)})
	if isErr {
		t.Fatalf("raced upload errored: %+v", out)
	}
	if out.Results[0].Status != "skipped" {
		t.Fatalf("status = %s, want skipped via compare", out.Results[0].Status)
	}
}

// streamFailFetcher wraps a real fetcher whose ConsumeBody fails the first
// N calls with a raw storage-looking error mid-stream (bytes already
// teed), proving the from-zero re-GET recovery (§8).
type streamFailFetcher struct {
	*imagedownload.Fetcher
	failN int32 // atomic
}

func (f *streamFailFetcher) ConsumeBody(resp *imagedownload.Response, sink io.Writer) (int64, imagedownload.Detected, error) {
	if n := atomic.AddInt32(&f.failN, -1); n >= 0 {
		_, _ = sink.Write([]byte("partial"))
		return 7, imagedownload.Detected{}, errors.New("synthetic storage failure: simulated ENOSPC-class write error")
	}
	return f.Fetcher.ConsumeBody(resp, sink)
}

func TestUploadStorageFailureReDownloads(t *testing.T) {
	cs, conn, dir, rt := startUploadServer(t)
	rt.responses["/a.png"] = rtResp(200, upPNG, nil)
	f := imagedownload.NewFetcher("upload-test/1.0")
	f.RoundTripper = rt
	conn.Fetch = &streamFailFetcher{Fetcher: f, failN: 1}

	out, isErr := uploadCall(t, cs, []any{item("/a.png", filepath.Join(dir, "a.png"))})
	if isErr {
		t.Fatalf("storage-failure recovery failed: %+v", out)
	}
	if out.Results[0].Status != "created" || rt.tripsNow() != 2 {
		t.Fatalf("result %+v after %d trips, want created after exactly 2 fetches", out.Results[0], rt.tripsNow())
	}
	got, _ := os.ReadFile(filepath.Join(dir, "a.png"))
	if !bytes.Equal(got, upPNG) {
		t.Error("re-downloaded bytes differ (partial temp was resumed?)")
	}
}

func TestUploadFreshnessForgetTextMarker(t *testing.T) {
	cs, _, dir, rt := startUploadServer(t)
	rt.responses["/a.png"] = rtResp(200, upPNG, nil)
	target := filepath.Join(dir, "p.png")

	// Read a text file at the path (marker set), then it disappears out of
	// band and upload re-creates an image there: the stale text marker
	// must be dropped, so the next edit demands a fresh read.
	writeDisk(t, target, "old text\n", 0o644)
	readTool(t, cs, target, nil)
	_ = os.Remove(target)

	if _, isErr := uploadCall(t, cs, []any{item("/a.png", target)}); isErr {
		t.Fatal("upload after external delete failed")
	}
	wantErr(t, cs, "edit", map[string]any{
		"file_path": target, "old_string": "x", "new_string": "y",
	}, "has not been read")
}

func TestUploadConcurrencyCaps(t *testing.T) {
	cs, conn, dir, rt := startUploadServer(t)
	release := make(chan struct{})
	var inflight, maxInflight int32
	rt.onTrip = func() {
		cur := atomic.AddInt32(&inflight, 1)
		for {
			old := atomic.LoadInt32(&maxInflight)
			if cur <= old || atomic.CompareAndSwapInt32(&maxInflight, old, cur) {
				break
			}
		}
		// Hold the wire until released.
		<-release
		atomic.AddInt32(&inflight, -1)
	}
	var files []any
	for i := 0; i < 8; i++ {
		rt.responses["/"+string(rune('a'+i))+".png"] = rtResp(200, upPNG, nil)
		files = append(files, item("/"+string(rune('a'+i))+".png", filepath.Join(dir, string(rune('a'+i))+".png")))
	}

	done := make(chan UploadOutput, 1)
	go func() {
		out, _ := uploadCall(t, cs, files)
		done <- out
	}()
	// Wait until the per-batch worker cap is parked on the wire, then let
	// everything through.
	deadline := time.Now().Add(10 * time.Second)
	for atomic.LoadInt32(&inflight) < int32(conn.UploadLimits.MaxConcurrentPerBatch) {
		if time.Now().After(deadline) {
			t.Fatalf("only %d downloads parked on the wire, want %d",
				atomic.LoadInt32(&inflight), conn.UploadLimits.MaxConcurrentPerBatch)
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(release)
	out := <-done
	if out.Summary.Created != 8 {
		t.Fatalf("summary = %+v, want 8 created", out.Summary)
	}
	if m := atomic.LoadInt32(&maxInflight); m > int32(conn.UploadLimits.MaxConcurrentPerBatch) {
		t.Errorf("max concurrent downloads = %d, want <= %d", m, conn.UploadLimits.MaxConcurrentPerBatch)
	}
}

func TestUploadBusyGate(t *testing.T) {
	cs, conn, dir, rt := startUploadServer(t)
	rt.responses["/a.png"] = rtResp(200, upPNG, nil)

	// Occupy every batch slot; the next call must EBusy immediately.
	for i := 0; i < conn.UploadLimits.MaxConcurrentBatches; i++ {
		conn.batchSem <- struct{}{}
	}
	defer func() {
		for i := 0; i < conn.UploadLimits.MaxConcurrentBatches; i++ {
			<-conn.batchSem
		}
	}()
	text, isErr := callTool(t, cs, "upload", map[string]any{"files": []any{
		item("/a.png", filepath.Join(dir, "a.png"))}})
	if !isErr || !strings.Contains(text, "EBusy") && !strings.Contains(text, "concurrent upload batches") {
		t.Errorf("busy gate = (%q, %v)", text, isErr)
	}
}

func TestUploadSVGCreated(t *testing.T) {
	cs, _, dir, rt := startUploadServer(t)
	rt.responses["/b.svg"] = rtResp(200, upSVG, http.Header{"Content-Type": []string{"image/svg+xml"}})
	target := filepath.Join(dir, "b.svg")

	out, isErr := uploadCall(t, cs, []any{item("/b.svg", target)})
	if isErr {
		t.Fatalf("svg upload failed: %+v", out)
	}
	if out.Results[0].ContentType != "image/svg+xml" {
		t.Errorf("content_type = %s", out.Results[0].ContentType)
	}
	got, _ := os.ReadFile(target)
	if !bytes.Equal(got, upSVG) {
		t.Error("svg bytes not stored verbatim")
	}
}
