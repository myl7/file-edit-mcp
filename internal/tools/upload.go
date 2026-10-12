// upload tool handler (ARCHITECTURE §5.9): batch shape validation, the
// process-wide batch/download gates, per-item download→stream→compare/
// publish pipeline, and the result aggregation contract (every input item
// answered; IsError exactly when any item failed; full results always).

package tools

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/myl7/file-edit-mcp/internal/errmsg"
	"github.com/myl7/file-edit-mcp/internal/imagedownload"
)

// acceptedExts is the §5.9 extension allowlist (jpg/jpeg alias included);
// a target outside it fails before any byte is fetched.
var acceptedExts = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true,
	".webp": true, ".avif": true, ".svg": true,
}

// extMatchesDetected checks the §5.9 extension/type agreement: jpg/jpeg
// alias for JPEG, exact canonical extension otherwise.
func extMatchesDetected(ext string, det imagedownload.Detected) bool {
	if det.MIME == "image/jpeg" {
		return ext == ".jpg" || ext == ".jpeg"
	}
	return ext == det.Ext
}

// Upload implements the upload tool (§5.9). Call-level failures (batch
// shape, EBusy) are plain errors — the SDK turns them into IsError results
// with the catalog text. Once items are dispatched, every failure is
// per-item: the call returns an explicit IsError result (true exactly when
// some item failed) alongside the complete results/summary, so partial
// failure never drops the item list.
func (s *Conn) Upload(ctx context.Context, _ *mcp.CallToolRequest, in UploadInput) (*mcp.CallToolResult, UploadOutput, error) {
	L := s.UploadLimits
	if len(in.Files) == 0 {
		return nil, UploadOutput{}, errmsg.EInvalidBatch("files must contain at least 1 item")
	}
	if len(in.Files) > L.MaxBatchItems {
		return nil, UploadOutput{}, errmsg.EInvalidBatch(
			"files must contain at most " + strconv.Itoa(L.MaxBatchItems) + " items, got " + strconv.Itoa(len(in.Files)))
	}
	for i, it := range in.Files {
		switch {
		case it.URL == "":
			return nil, UploadOutput{}, errmsg.EInvalidBatch("item " + strconv.Itoa(i+1) + ": url is empty")
		case len(it.URL) > L.MaxURLBytes:
			return nil, UploadOutput{}, errmsg.EInvalidBatch("item " + strconv.Itoa(i+1) + ": url exceeds the length limit")
		case it.FilePath == "":
			return nil, UploadOutput{}, errmsg.EInvalidBatch("item " + strconv.Itoa(i+1) + ": file_path is empty")
		}
	}

	// §5.9 batch gate: no queueing — a full process budget refuses with
	// EBusy before anything starts.
	select {
	case s.batchSem <- struct{}{}:
		defer func() { <-s.batchSem }()
	default:
		return nil, UploadOutput{}, errmsg.EBusy()
	}

	batchCtx, cancel := context.WithTimeout(ctx, L.BatchBudget)
	defer cancel()

	// Pre-validate every path and mark duplicates; both are per-item
	// failures, not call-level ones (§5.9).
	results := make([]UploadItemResult, len(in.Files))
	resolved := make([]string, len(in.Files))
	byTarget := make(map[string][]int, len(in.Files))
	for i, it := range in.Files {
		r, err := s.Guard.Validate(it.FilePath)
		if err != nil {
			results[i] = itemFailed(i+1, it.FilePath, err)
			continue
		}
		resolved[i] = r
		byTarget[r] = append(byTarget[r], i)
	}
	for target, idxs := range byTarget {
		if len(idxs) > 1 {
			for _, i := range idxs {
				results[i] = itemFailed(i+1, in.Files[i].FilePath, errmsg.EDuplicateTarget(target))
				resolved[i] = ""
			}
		}
	}

	// Workers: bounded by the per-batch concurrency (the process-wide
	// download semaphore inside uploadItem is the real gate); items not
	// started before the batch deadline answer ETimeout.
	workers := L.MaxConcurrentPerBatch
	if workers > len(in.Files) {
		workers = len(in.Files)
	}
	if workers < 1 {
		workers = 1
	}
	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				if batchCtx.Err() != nil {
					results[i] = itemFailed(i+1, in.Files[i].FilePath, errmsg.ETimeout("batch"))
					continue
				}
				start := time.Now()
				res := s.uploadItem(batchCtx, i+1, in.Files[i], resolved[i])
				s.logItem(i+1, resolved[i], res, time.Since(start))
				results[i] = res
			}
		}()
	}
	for i := range in.Files {
		if resolved[i] != "" && results[i].Status == "" {
			jobs <- i
		}
	}
	close(jobs)
	wg.Wait()

	out := UploadOutput{Results: results}
	for _, r := range results {
		switch r.Status {
		case "created":
			out.Summary.Created++
		case "skipped":
			out.Summary.Skipped++
		default:
			out.Summary.Failed++
		}
	}
	// §5.9 (revised): any failed item makes the call an error result —
	// with the full results attached, never a bare error.
	return &mcp.CallToolResult{IsError: out.Summary.Failed > 0}, out, nil
}

// uploadItem runs one item end to end (§5.9): download slot → bounded
// download+stream (from-zero re-GET on storage failure) → extension/type
// agreement → §7 locks → compare/publish → result row. index is the
// 1-based input position carried into the result row.
func (s *Conn) uploadItem(batchCtx context.Context, index int, item UploadItem, resolved string) UploadItemResult {
	fail := func(err error) UploadItemResult {
		return itemFailed(index, resolved, err)
	}

	ext := strings.ToLower(filepath.Ext(resolved))
	if !acceptedExts[ext] {
		return fail(errmsg.EImageType("target extension " + ext + " is not an accepted image extension"))
	}

	// Download slot: the item budget starts here (§5.9).
	select {
	case s.downloadSem <- struct{}{}:
		defer func() { <-s.downloadSem }()
	case <-batchCtx.Done():
		return fail(errmsg.ETimeout("download queue"))
	}
	itemCtx, cancel := context.WithTimeout(batchCtx, s.UploadLimits.ItemBudget)
	defer cancel()

	var dl *downloaded
	for {
		resp, err := s.Fetch.Get(itemCtx, item.URL)
		if err != nil {
			return fail(mapFetchErr(itemCtx, err, s.UploadLimits.MaxImageBytes))
		}
		dl, err = s.streamOnce(itemCtx, resolved, resp)
		_ = resp.Body.Close()
		if err == nil {
			break
		}
		if isStorageStreamErr(err) && itemCtx.Err() == nil {
			// §8: storage failure mid-stream — close, clean, re-GET from
			// zero under the same budget (never resume a half-written
			// temp). streamOnce already removed its temp.
			s.log().Warn("upload storage failure; re-downloading from zero",
				"path", resolved, "origin", originOf(resp), "err", err)
			continue
		}
		return fail(mapStreamErr(itemCtx, err, s.UploadLimits.MaxImageBytes))
	}

	// §5.9 extension agreement — after content detection, never instead of
	// it. A mismatch answers with the detected type and a suggested
	// extension so the caller can pick a new path.
	if !extMatchesDetected(ext, dl.det) {
		s.cleanupTemp(resolved, dl.tmpRel)
		res := fail(errmsg.EImageType("detected " + dl.det.MIME + " does not match extension " + ext))
		res.DetectedContentType = dl.det.MIME
		res.SuggestedExtension = dl.det.Ext
		return res
	}

	// §7 locks, acquired AFTER the download (§5.9): waiting is cancellable
	// so a slow network never blocks a lock holder's budget.
	release, err := s.Shared.plocks.LockContext(itemCtx, resolved)
	if err != nil {
		s.cleanupTemp(resolved, dl.tmpRel)
		return fail(errmsg.ETimeout("publish lock"))
	}
	defer release()
	unlock := s.Sess.LockFile(resolved)
	defer unlock()

	status, warning, err := s.publishItem(itemCtx, resolved, dl)
	if err != nil {
		s.cleanupTemp(resolved, dl.tmpRel)
		return fail(mapPublishErr(itemCtx, err))
	}
	if status == "created" {
		// §5.9 freshness: a created file is unknown content to this token
		// (and the hash grants no write knowledge) — drop any stale marker
		// so a later text edit must read first.
		s.Sess.Forget(resolved)
	}
	res := UploadItemResult{
		Index:       index,
		FilePath:    resolved,
		Status:      status,
		Bytes:       dl.size,
		SHA256:      dl.sha,
		ContentType: dl.det.MIME,
	}
	if warning != "" {
		res.WarningCode = warning
	}
	return res
}

// itemFailed renders a catalog error as a failed item row.
func itemFailed(index int, path string, err error) UploadItemResult {
	return UploadItemResult{
		Index:     index,
		FilePath:  path,
		Status:    "failed",
		ErrorCode: errmsg.Code(err),
		Error:     err.Error(),
	}
}

// mapFetchErr maps imagedownload's taxonomy onto the errmsg rows (§5.9).
func mapFetchErr(ctx context.Context, err error, maxBytes int64) error {
	var ie *imagedownload.Error
	if errors.As(err, &ie) {
		switch ie.Kind {
		case imagedownload.ErrBadURL:
			return errmsg.EInvalidURL(ie.Detail)
		case imagedownload.ErrBlocked:
			return errmsg.ESSRFBlocked(ie.Detail)
		case imagedownload.ErrRedirects:
			return errmsg.ERedirectLimit()
		case imagedownload.ErrStatus:
			return errmsg.EHTTPStatus(ie.Status)
		case imagedownload.ErrNet:
			return errmsg.EDownload(ie.Detail)
		case imagedownload.ErrTimeout:
			return errmsg.ETimeout(ie.Detail)
		case imagedownload.ErrTooLarge:
			return errmsg.ETooLarge(maxBytes)
		case imagedownload.ErrType:
			return errmsg.EImageType(ie.Detail)
		}
	}
	if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		return errmsg.ETimeout("download")
	}
	return errmsg.EDownload("request")
}

// mapStreamErr maps streaming failures: content/network taxonomy rows pass
// through mapFetchErr's vocabulary; raw storage errors keep their §8
// classification (EBackend after retries — handled by the caller's
// runFSContext in the temp stage — or the raw error text as a last
// resort).
func mapStreamErr(ctx context.Context, err error, maxBytes int64) error {
	var ie *imagedownload.Error
	if errors.As(err, &ie) {
		return mapFetchErr(ctx, ie, maxBytes)
	}
	if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		return errmsg.ETimeout("download")
	}
	// Storage errors that exhausted the re-GET budget or hit a definite
	// errno: wrap as EBackend so the model sees the §8 storage wording.
	return errmsg.EBackend(err)
}

// mapPublishErr maps compare/publish failures; context expiry inside the
// §8 retry is the ETimeout row.
func mapPublishErr(ctx context.Context, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return errmsg.ETimeout("publish")
	}
	return err // already catalog rows (EExistsConflict, EIsDir, ...) or EBackend-wrapped
}

// logItem writes the per-item server log line (§5.9): index, target,
// status/code, duration, bytes, and the origin (scheme://host only — the
// query can carry signatures and never reaches logs).
func (s *Conn) logItem(index int, path string, res UploadItemResult, dur time.Duration) {
	if path == "" {
		return
	}
	s.log().Info("upload item",
		"index", index, "path", path, "status", res.Status,
		"code", res.ErrorCode, "bytes", res.Bytes, "dur", dur.Milliseconds())
}
