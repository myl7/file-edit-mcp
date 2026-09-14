package cifsops

import (
	"errors"
	"fmt"
	"io/fs"
	"reflect"
	"sync"
	"syscall"
	"testing"
	"time"
)

// --- classification -------------------------------------------------------

func TestClassify(t *testing.T) {
	perr := func(e syscall.Errno) error {
		// *fs.PathError is how the os package surfaces errnos.
		return &fs.PathError{Op: "open", Path: "/data/x", Err: e}
	}
	cases := []struct {
		name string
		err  error
		want Class
	}{
		{"nil", nil, ClassUnknown},
		{"plain error", errors.New("boom"), ClassUnknown},
		{"EPERM not in any bucket", perr(syscall.EPERM), ClassUnknown},
		{"bare EIO", syscall.EIO, ClassBackend},
		{"ENOENT", perr(syscall.ENOENT), ClassDefinitive},
		{"ENOTDIR", perr(syscall.ENOTDIR), ClassDefinitive},
		{"EISDIR", perr(syscall.EISDIR), ClassDefinitive},
		{"ENAMETOOLONG", perr(syscall.ENAMETOOLONG), ClassDefinitive},
		{"EIO", perr(syscall.EIO), ClassBackend},
		{"ETIMEDOUT", perr(syscall.ETIMEDOUT), ClassBackend},
		{"EHOSTDOWN", perr(syscall.EHOSTDOWN), ClassBackend},
		{"ENOTCONN", perr(syscall.ENOTCONN), ClassBackend},
		{"ESTALE", perr(syscall.ESTALE), ClassBackend},
		{"ECONNRESET", perr(syscall.ECONNRESET), ClassBackend},
		{"EAGAIN", perr(syscall.EAGAIN), ClassBackend},
		{"double-wrapped ESTALE", fmt.Errorf("stat root: %w", perr(syscall.ESTALE)), ClassBackend},
	}
	for _, tc := range cases {
		if got := Classify(tc.err); got != tc.want {
			t.Errorf("Classify(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// --- test fakes ------------------------------------------------------------

// fakeSleeper records sleeps instead of waiting.
type fakeSleeper struct {
	mu     sync.Mutex
	delays []time.Duration
}

func (f *fakeSleeper) sleep(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delays = append(f.delays, d)
}

func (f *fakeSleeper) recorded() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Duration(nil), f.delays...)
}

// seqOp returns errs[i] on the i-th call and nil once the list is spent.
// The exact call count is asserted by each test, so an over-retry fails.
type seqOp struct {
	mu    sync.Mutex
	errs  []error
	calls int
}

func (o *seqOp) op() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	i := o.calls
	o.calls++
	if i < len(o.errs) {
		return o.errs[i]
	}
	return nil
}

func (o *seqOp) callCount() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.calls
}

func backendErr() error {
	return &fs.PathError{Op: "stat", Path: "/data/x", Err: syscall.EIO}
}

func notExistErr() error {
	return &fs.PathError{Op: "open", Path: "/data/x", Err: syscall.ENOENT}
}

// --- retry policy ----------------------------------------------------------

func TestDoSuccessFirstTry(t *testing.T) {
	f := &fakeSleeper{}
	r := Retrier{Sleep: f.sleep}
	op := &seqOp{}
	if err := r.Do(op.op); err != nil {
		t.Fatalf("Do = %v, want nil", err)
	}
	if op.callCount() != 1 {
		t.Errorf("op calls = %d, want 1", op.callCount())
	}
	if sleeps := f.recorded(); len(sleeps) != 0 {
		t.Errorf("sleeps = %v, want none", sleeps)
	}
	if !r.succeeded {
		t.Error("succeeded not set after first-try success")
	}
}

func TestDoColdRetriesThenSuccess(t *testing.T) {
	f := &fakeSleeper{}
	r := Retrier{Sleep: f.sleep}
	op := &seqOp{errs: []error{backendErr(), backendErr()}}
	if err := r.Do(op.op); err != nil {
		t.Fatalf("Do = %v, want nil", err)
	}
	if op.callCount() != 3 {
		t.Errorf("op calls = %d, want 3 (2 backend failures + 1 success)", op.callCount())
	}
	want := []time.Duration{500 * time.Millisecond, 2 * time.Second}
	if got := f.recorded(); !reflect.DeepEqual(got, want) {
		t.Errorf("sleeps = %v, want %v", got, want)
	}
	if !r.succeeded {
		t.Error("succeeded not set after successful cold retry")
	}
}

func TestDoColdExhausts(t *testing.T) {
	f := &fakeSleeper{}
	var attempts []int
	r := Retrier{
		Sleep: f.sleep,
		OnRetry: func(attempt int) error {
			attempts = append(attempts, attempt)
			return nil
		},
	}
	op := &seqOp{errs: []error{backendErr(), backendErr(), backendErr(), backendErr()}}
	err := r.Do(op.op)
	if !errors.Is(err, syscall.EIO) {
		t.Fatalf("Do err = %v, want the raw EIO backend error", err)
	}
	if op.callCount() != 4 {
		t.Errorf("op calls = %d, want 4 (initial + 3 cold retries)", op.callCount())
	}
	wantSleeps := []time.Duration{500 * time.Millisecond, 2 * time.Second, 5 * time.Second}
	if got := f.recorded(); !reflect.DeepEqual(got, wantSleeps) {
		t.Errorf("sleeps = %v, want %v", got, wantSleeps)
	}
	if !reflect.DeepEqual(attempts, []int{1, 2, 3}) {
		t.Errorf("OnRetry attempts = %v, want [1 2 3]", attempts)
	}
	if r.succeeded {
		t.Error("succeeded set despite exhaustion, want false")
	}
}

// warmRetrier returns a retrier already flipped warm by a successful op.
func warmRetrier(f *fakeSleeper, onRetry func(attempt int) error) *Retrier {
	r := &Retrier{Sleep: f.sleep, OnRetry: onRetry}
	if err := r.Do(func() error { return nil }); err != nil {
		panic(err)
	}
	f.mu.Lock()
	f.delays = nil // drop anything the warm-up could have recorded
	f.mu.Unlock()
	return r
}

func TestDoWarmRetrySucceeds(t *testing.T) {
	f := &fakeSleeper{}
	retries := 0
	r := warmRetrier(f, func(attempt int) error {
		retries++
		if attempt != 1 {
			t.Errorf("OnRetry attempt = %d, want 1", attempt)
		}
		return nil
	})
	op := &seqOp{errs: []error{backendErr()}}
	if err := r.Do(op.op); err != nil {
		t.Fatalf("Do = %v, want nil", err)
	}
	if op.callCount() != 2 {
		t.Errorf("op calls = %d, want 2 (backend failure + single warm retry)", op.callCount())
	}
	if got := f.recorded(); len(got) != 1 || got[0] != WarmRetryDelay {
		t.Errorf("sleeps = %v, want [%v]", got, WarmRetryDelay)
	}
	if retries != 1 {
		t.Errorf("OnRetry calls = %d, want exactly 1", retries)
	}
}

func TestDoWarmExhaustsAfterSingleRetry(t *testing.T) {
	f := &fakeSleeper{}
	attempts := []int{}
	r := warmRetrier(f, func(attempt int) error {
		attempts = append(attempts, attempt)
		return nil
	})
	op := &seqOp{errs: []error{backendErr(), backendErr(), backendErr()}}
	err := r.Do(op.op)
	if !errors.Is(err, syscall.EIO) {
		t.Fatalf("Do err = %v, want the raw EIO backend error", err)
	}
	if op.callCount() != 2 {
		t.Errorf("op calls = %d, want 2 (warm path retries exactly once)", op.callCount())
	}
	if got := f.recorded(); len(got) != 1 || got[0] != WarmRetryDelay {
		t.Errorf("sleeps = %v, want [%v]", got, WarmRetryDelay)
	}
	if !reflect.DeepEqual(attempts, []int{1}) {
		t.Errorf("OnRetry attempts = %v, want [1]", attempts)
	}
}

func TestDoWarmWithoutOnRetryHook(t *testing.T) {
	f := &fakeSleeper{}
	r := warmRetrier(f, nil) // OnRetry default nil must not panic
	op := &seqOp{errs: []error{backendErr()}}
	if err := r.Do(op.op); err != nil {
		t.Fatalf("Do = %v, want nil", err)
	}
	if op.callCount() != 2 {
		t.Errorf("op calls = %d, want 2", op.callCount())
	}
	if got := f.recorded(); len(got) != 1 || got[0] != WarmRetryDelay {
		t.Errorf("sleeps = %v, want [%v]", got, WarmRetryDelay)
	}
}

func TestDoDefinitiveNeverRetried(t *testing.T) {
	for _, warm := range []bool{false, true} {
		f := &fakeSleeper{}
		var r *Retrier
		if warm {
			r = warmRetrier(f, nil)
		} else {
			r = &Retrier{Sleep: f.sleep}
		}
		op := &seqOp{errs: []error{notExistErr(), notExistErr()}}
		err := r.Do(op.op)
		if !errors.Is(err, syscall.ENOENT) {
			t.Fatalf("warm=%v: Do err = %v, want the raw ENOENT", warm, err)
		}
		if op.callCount() != 1 {
			t.Errorf("warm=%v: op calls = %d, want 1 (ENOENT is never retried)", warm, op.callCount())
		}
		if sleeps := f.recorded(); len(sleeps) != 0 {
			t.Errorf("warm=%v: sleeps = %v, want none", warm, sleeps)
		}
	}
}

func TestDoUnknownNeverRetried(t *testing.T) {
	f := &fakeSleeper{}
	r := warmRetrier(f, nil) // even warm: unknown errors get no retry
	op := &seqOp{errs: []error{errors.New("plain failure"), errors.New("plain failure")}}
	err := r.Do(op.op)
	if err == nil || err.Error() != "plain failure" {
		t.Fatalf("Do err = %v, want the raw plain error", err)
	}
	if op.callCount() != 1 {
		t.Errorf("op calls = %d, want 1 (unknown class is never retried)", op.callCount())
	}
	if sleeps := f.recorded(); len(sleeps) != 0 {
		t.Errorf("sleeps = %v, want none", sleeps)
	}
}

func TestDoStopsRetryingWhenErrorTurnsDefinitive(t *testing.T) {
	f := &fakeSleeper{}
	r := Retrier{Sleep: f.sleep}
	// First EIO (backend, retry), then the remount reveals ENOENT.
	op := &seqOp{errs: []error{backendErr(), notExistErr(), notExistErr()}}
	err := r.Do(op.op)
	if !errors.Is(err, syscall.ENOENT) {
		t.Fatalf("Do err = %v, want the raw ENOENT after mid-sequence switch", err)
	}
	if op.callCount() != 2 {
		t.Errorf("op calls = %d, want 2", op.callCount())
	}
}

func TestDoOnRetryErrorAborts(t *testing.T) {
	f := &fakeSleeper{}
	hookErr := errors.New("root reopen failed")
	r := Retrier{
		Sleep:   f.sleep,
		OnRetry: func(int) error { return hookErr },
	}
	op := &seqOp{errs: []error{backendErr(), backendErr()}}
	err := r.Do(op.op)
	if !errors.Is(err, hookErr) {
		t.Fatalf("Do err = %v, want the OnRetry error", err)
	}
	if op.callCount() != 1 {
		t.Errorf("op calls = %d, want 1 (no retry after failed reopen)", op.callCount())
	}
	if sleeps := f.recorded(); len(sleeps) != 0 {
		t.Errorf("sleeps = %v, want none (abort happens before sleeping)", sleeps)
	}
}

func TestZeroValueRetrierUsesRealSleepContract(t *testing.T) {
	// A succeeding op on the zero value must not sleep, so this test does
	// not actually wait even though Sleep is time.Sleep.
	var r Retrier
	if err := r.Do(func() error { return nil }); err != nil {
		t.Fatalf("Do = %v, want nil", err)
	}
	if !r.succeeded {
		t.Error("succeeded not set by zero-value Retrier")
	}
}

func TestConcurrentDo(t *testing.T) {
	f := &fakeSleeper{}
	r := Retrier{Sleep: f.sleep}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				if err := r.Do(func() error { return nil }); err != nil {
					t.Errorf("success Do = %v", err)
				}
				return
			}
			// Always-failing op: raw EIO back, no panic. Depending on
			// interleaving it runs cold (4 calls) or gets flipped warm
			// by a concurrent success and runs the single-retry path
			// (2 calls) — both are correct per §8.
			n := 0
			err := r.Do(func() error { n++; return backendErr() })
			if !errors.Is(err, syscall.EIO) {
				t.Errorf("failing Do = %v, want EIO", err)
			}
			if n != 4 && n != 2 {
				t.Errorf("failing op calls = %d, want 4 (cold) or 2 (flipped warm)", n)
			}
		}(i)
	}
	wg.Wait()
	if !r.succeeded {
		t.Error("succeeded not set despite successful concurrent ops")
	}
}
