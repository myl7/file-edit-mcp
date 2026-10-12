// Package cifsops classifies filesystem errors and drives the retry policy
// for the CIFS soft mount (ARCHITECTURE §8).
//
// Classification splits errnos into three classes:
//
//   - Definitive: a real filesystem answer (ENOENT, ENOTDIR, EISDIR,
//     ENAMETOOLONG) — never retried, reported as-is.
//   - Backend: mount jitter or cold automount wake-up (EIO, ETIMEDOUT,
//     EHOSTDOWN, ENOTCONN, ESTALE, ECONNRESET, EAGAIN) — retried per the
//     policy below, and always reported separately from ENOENT: backend
//     errors suggest a retry, ENOENT must never suggest one.
//   - Unknown: anything else — never retried.
//
// Retry policy (§8): a cold process (no successful fs operation yet)
// retries backend errors up to ColdMaxRetries times, sleeping
// ColdBackoff[i] before retry i. A warm process retries a backend error
// once, after the OnRetry hook (reopen the os.Root to re-trigger the
// automount) and WarmRetryDelay. Definitive and unknown errors are returned
// immediately. Final failures are returned as the raw error; wrapping them
// into the EBackend message is the caller's job, outside this package.
package cifsops

import (
	"context"
	"errors"
	"sync"
	"syscall"
	"time"
)

// Class is the retry-relevant classification of an error.
type Class int

const (
	// ClassDefinitive marks real filesystem semantics; never retried.
	ClassDefinitive Class = iota
	// ClassBackend marks mount jitter / cold start; retried per §8.
	ClassBackend
	// ClassUnknown marks everything else; never retried.
	ClassUnknown
)

// String names the class for logs and test failures.
func (c Class) String() string {
	switch c {
	case ClassDefinitive:
		return "definitive"
	case ClassBackend:
		return "backend"
	default:
		return "unknown"
	}
}

// The errno buckets of §8. They must stay disjoint: ENOENT and friends are
// filesystem answers, the backend set is transport/mount trouble.
var (
	definitiveErrnos = []syscall.Errno{
		syscall.ENOENT,
		syscall.ENOTDIR,
		syscall.EISDIR,
		syscall.ENAMETOOLONG,
	}
	backendErrnos = []syscall.Errno{
		syscall.EIO,
		syscall.ETIMEDOUT,
		syscall.EHOSTDOWN,
		syscall.ENOTCONN,
		syscall.ESTALE,
		syscall.ECONNRESET,
		syscall.EAGAIN,
	}
)

// Classify inspects err — unwrapping with errors.As down to a
// *syscall.Errno — and returns its Class. Nil errors and errors without an
// errno classify as Unknown.
func Classify(err error) Class {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return ClassUnknown
	}
	for _, e := range definitiveErrnos {
		if errno == e {
			return ClassDefinitive
		}
	}
	for _, e := range backendErrnos {
		if errno == e {
			return ClassBackend
		}
	}
	return ClassUnknown
}

// Retry-policy constants, centrally defined (§8).
const (
	// ColdMaxRetries is the maximum number of retries during cold start.
	ColdMaxRetries = 3
	// WarmRetryDelay is the pause before the single warm-state retry.
	WarmRetryDelay = 800 * time.Millisecond
)

// ColdBackoff lists the sleeps before each cold-start retry (500ms / 2s / 5s).
var ColdBackoff = [ColdMaxRetries]time.Duration{
	500 * time.Millisecond,
	2 * time.Second,
	5 * time.Second,
}

// Retrier implements the §8 retry policy around one-shot fs operations.
//
// The zero value is ready to use: cold, no OnRetry hook, real time.Sleep.
// Configure OnRetry/Sleep before first use; do not mutate them while Do is
// running. A Retrier is safe for concurrent use (the succeeded flag is
// lock-guarded), though retries from concurrent Do calls may overlap —
// callers that need per-path serialization hold session.LockFile around
// the whole Do call.
type Retrier struct {
	// OnRetry, when non-nil, runs before every retry attempt (attempt is
	// 1-based: 1..3 cold, 1 warm) so the caller can close and reopen the
	// os.Root and re-trigger the automount. If it returns an error, Do
	// aborts immediately and returns that error — with a broken Root the
	// retry is pointless.
	OnRetry func(attempt int) error

	// Sleep pauses between attempts. Defaults to time.Sleep; tests inject
	// a recorder so nothing actually waits.
	Sleep func(time.Duration)

	// CtxSleep pauses between attempts for DoContext, cancellably.
	// Defaults to a timer + select on ctx.Done; tests inject a recorder.
	// Do (the context-free form) never consults it.
	CtxSleep func(ctx context.Context, d time.Duration) error

	mu        sync.Mutex
	succeeded bool
}

// Do runs op under the §8 retry policy.
//
// op returning nil marks the process warm (succeeded) and returns nil.
// Definitive and unknown errors return immediately, raw and unretried.
// Backend errors retry: cold, up to ColdMaxRetries times with the
// ColdBackoff sleeps; warm, exactly once after OnRetry (if set) and
// WarmRetryDelay. On exhaustion the last backend error is returned raw —
// the caller (outside this package) wraps it in the EBackend message.
func (r *Retrier) Do(op func() error) error {
	sleepFor := r.sleepFunc()
	return r.do(context.Background(), op, func(_ context.Context, d time.Duration) error {
		sleepFor(d)
		return nil
	})
}

// DoContext is Do with a cancellable context (§8, the upload/mkdir form):
// the policy is identical, but cancellation is checked before the first
// attempt, before every OnRetry hook, and during every backoff sleep — a
// canceled or deadline-expired context aborts the wait loop immediately
// with ctx.Err() and never runs another op() attempt. The long-budget
// handlers (mkdir, upload) use this so their deadlines actually fence the
// retry machinery; Do keeps its historical context-free behavior for the
// existing tools.
func (r *Retrier) DoContext(ctx context.Context, op func() error) error {
	return r.do(ctx, op, r.ctxSleepFunc())
}

// do is the shared Do/DoContext core: run op; on a backend-class error,
// retry per the cold/warm backoff schedule with OnRetry before each
// attempt. sleep gates each wait and returns a non-nil error only when the
// wait was cancelled (the DoContext form); the plain Do form passes a
// wrapper around the classic Sleep field that never cancels.
func (r *Retrier) do(ctx context.Context, op func() error, sleep func(context.Context, time.Duration) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	err := op()
	if err == nil {
		r.markSucceeded()
		return nil
	}
	if Classify(err) != ClassBackend {
		return err
	}

	var backoff []time.Duration
	if r.warm() {
		backoff = []time.Duration{WarmRetryDelay}
	} else {
		backoff = ColdBackoff[:]
	}
	for attempt, delay := range backoff {
		if err := ctx.Err(); err != nil {
			return err
		}
		if r.OnRetry != nil {
			if hookErr := r.OnRetry(attempt + 1); hookErr != nil {
				return hookErr
			}
		}
		if err := sleep(ctx, delay); err != nil {
			return err
		}
		err = op()
		if err == nil {
			r.markSucceeded()
			return nil
		}
		if Classify(err) != ClassBackend {
			return err
		}
	}
	return err
}

// warm reports whether any operation has already succeeded in this process.
func (r *Retrier) warm() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.succeeded
}

// markSucceeded flips the process to warm; it never flips back (§8 defines
// cold as "no successful fs operation yet").
func (r *Retrier) markSucceeded() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.succeeded = true
}

// sleepFunc resolves the injected or default sleep function once per Do.
func (r *Retrier) sleepFunc() func(time.Duration) {
	if r.Sleep != nil {
		return r.Sleep
	}
	return time.Sleep
}

// ctxSleepFunc resolves the injected or default cancellable sleep used by
// DoContext. The default waits on a timer raced against ctx.Done, so a
// canceled context never waits out a backoff interval.
func (r *Retrier) ctxSleepFunc() func(context.Context, time.Duration) error {
	if r.CtxSleep != nil {
		return r.CtxSleep
	}
	return sleepContext
}

// sleepContext waits for d unless ctx finishes first.
func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
