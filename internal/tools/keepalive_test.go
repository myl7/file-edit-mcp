// Keepalive tests (ARCHITECTURE §0): the HTTP-mode automount keepalive loop
// fires per interval through every allowed directory, survives probe
// failures (logged, never fatal), uses the real RootPool probe by default,
// stops with its context, and a non-positive interval disables it.

package tools

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a mutex-guarded bytes.Buffer: the keepalive goroutine logs
// into it while the test reads it, so plain bytes.Buffer would be a race.
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestKeepaliveFiresAndStops: with a short interval the probe runs
// repeatedly (interval-injected — this is the --keepalive test surface),
// sees every allowed dir, and stops once the context is canceled.
func TestKeepaliveFiresAndStops(t *testing.T) {
	dir := t.TempDir()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	sh, err := NewShared([]string{dir}, nil)
	if err != nil {
		t.Fatalf("NewShared: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	calls := make(chan string, 64)
	sh.Keepalive(ctx, 5*time.Millisecond, func(dir string) error {
		calls <- dir
		return nil
	})

	// At least three ticks must arrive promptly.
	seen := 0
	deadline := time.After(3 * time.Second)
	for seen < 3 {
		select {
		case got := <-calls:
			if got != real {
				t.Errorf("probe dir = %q, want the canonicalized allowed dir %q", got, real)
			}
			seen++
		case <-deadline:
			t.Fatalf("keepalive fired %d times in 3s, want >= 3", seen)
		}
	}

	cancel()
	// Drain window: a tick racing the cancel may still be mid-probe.
	time.Sleep(30 * time.Millisecond)
	for {
		select {
		case <-calls:
			continue
		default:
		}
		break
	}
	// Then the loop must be quiet.
	select {
	case got := <-calls:
		t.Errorf("keepalive fired after cancel: %q", got)
	case <-time.After(150 * time.Millisecond):
	}
}

// TestKeepaliveDisabled: interval <= 0 (--keepalive 0) never starts the
// loop.
func TestKeepaliveDisabled(t *testing.T) {
	sh, err := NewShared([]string{t.TempDir()}, nil)
	if err != nil {
		t.Fatalf("NewShared: %v", err)
	}
	calls := make(chan string, 1)
	sh.Keepalive(context.Background(), 0, func(dir string) error {
		calls <- dir
		return nil
	})
	select {
	case got := <-calls:
		t.Errorf("probe fired despite interval 0: %q", got)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestKeepaliveDefaultProbeQuiet: the production probe (nil) stats the
// allowed dir through the Root pool without logging — a live dir must
// produce no warnings.
func TestKeepaliveDefaultProbeQuiet(t *testing.T) {
	var buf syncBuffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	sh, err := NewShared([]string{t.TempDir()}, logger)
	if err != nil {
		t.Fatalf("NewShared: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sh.Keepalive(ctx, 5*time.Millisecond, nil)
	time.Sleep(40 * time.Millisecond)
	if out := buf.String(); strings.Contains(out, "keepalive stat failed") {
		t.Errorf("keepalive logged on a healthy dir: %q", out)
	}
}

// TestKeepaliveLogsFailures: a failing probe is logged at warn level and
// the loop keeps running (the next tick still fires) — §0: the loop must
// outlive transient mount trouble, never exit.
func TestKeepaliveLogsFailures(t *testing.T) {
	var buf syncBuffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	sh, err := NewShared([]string{t.TempDir()}, logger)
	if err != nil {
		t.Fatalf("NewShared: %v", err)
	}
	calls := make(chan struct{}, 64)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sh.Keepalive(ctx, 5*time.Millisecond, func(string) error {
		calls <- struct{}{}
		return context.DeadlineExceeded // any error shape
	})

	// Wait for the second call: proof the loop survived the first failure.
	for n := 0; n < 2; n++ {
		select {
		case <-calls:
		case <-time.After(3 * time.Second):
			t.Fatalf("keepalive stopped after failures; only %d calls", n)
		}
	}
	if out := buf.String(); !strings.Contains(out, "keepalive stat failed") {
		t.Errorf("failure log missing; got %q", out)
	}
}
