package ops

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestUnitReadinessFollowsCheck(t *testing.T) {
	var fail atomic.Bool
	changes := make(chan bool, 10)
	r := &Readiness{}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go r.Watch(ctx, 5*time.Millisecond, slog.New(slog.DiscardHandler), func(context.Context) error {
		if fail.Load() {
			return errors.New("db down")
		}
		return nil
	}, func(ok bool) { changes <- ok })

	if !<-changes {
		t.Fatal("first result must be reported: ready")
	}
	fail.Store(true)
	if <-changes {
		t.Fatal("want not ready after the check fails")
	}

	h := Handler(NewRegistry(), r)
	for path, want := range map[string]int{"/healthz": 200, "/readyz": 503, "/metrics": 200} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != want {
			t.Errorf("%s: %d, want %d", path, rec.Code, want)
		}
	}
}

// After Drain neither a passing check nor a stale result makes the process ready again:
// /readyz answers 503 for the whole SHUTDOWN_DELAY while liveness stays up.
func TestUnitDrainKeepsReadinessDownWhileTheCheckPasses(t *testing.T) {
	r := &Readiness{}
	changes := make(chan bool, 10)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Watch(ctx, 5*time.Millisecond, slog.New(slog.DiscardHandler), func(context.Context) error { return nil }, func(ok bool) { changes <- ok })
	}()
	if !<-changes || !r.Ready() {
		t.Fatal("passing check did not report ready")
	}
	h := Handler(NewRegistry(), r)
	status := func(path string) int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec.Code
	}
	if status("/readyz") != http.StatusOK {
		t.Fatal("ready process answered not ready")
	}
	r.Drain()
	if r.Ready() || status("/readyz") != http.StatusServiceUnavailable {
		t.Fatal("draining process still ready")
	}
	// Several more passing checks must not bring readiness back.
	deadline := time.After(50 * time.Millisecond)
	for wait := true; wait; {
		select {
		case ok := <-changes:
			if ok {
				t.Fatal("watch reported ready after drain")
			}
		case <-deadline:
			wait = false
		}
	}
	if r.Ready() || status("/readyz") != http.StatusServiceUnavailable || status("/healthz") != http.StatusOK {
		t.Fatal("drain did not hold or changed liveness")
	}
	cancel()
	<-done
}

// drainDuringNotification calls Drain while Watch is held inside a ready notification
// (entered is closed there; the notification waits for release) and returns the order
// of the delivered ready notification and Drain's return. Watch is released only once
// Drain has reached its lock (beforeDrainLock), so on the fixed code the interleaving
// is deterministic: Drain waits for the lock Watch holds and returns after "ready".
// A Drain that does not wait for the lock is caught with very high probability, not
// independently of the scheduler: it usually returns before the released Watch
// records "ready", and the callers' order check fails. No timer decides anything.
func drainDuringNotification(t *testing.T, r *Readiness, log *slog.Logger, entered <-chan struct{}, release chan struct{}, onChange func(bool)) []string {
	t.Helper()
	var (
		mu     sync.Mutex
		events []string
	)
	record := func(e string) { mu.Lock(); events = append(events, e); mu.Unlock() }
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		r.Watch(ctx, time.Millisecond, log, func(context.Context) error { return nil }, func(ok bool) {
			onChange(ok)
			if ok {
				record("ready")
			}
		})
	}()
	<-entered
	waiting := make(chan struct{})
	r.beforeDrainLock = func() { close(waiting) }
	drained := make(chan struct{})
	go func() { r.Drain(); record("drained"); close(drained) }()
	select {
	case <-waiting: // Drain now waits for the lock Watch holds during the notification
	case <-drained:
		close(release)
		cancel()
		<-watched
		t.Fatal("Drain returned while a ready notification was in flight")
	}
	close(release)
	<-drained
	cancel()
	<-watched
	mu.Lock()
	defer mu.Unlock()
	return append([]string(nil), events...)
}

// An onChange(true) already in flight when Drain is called finishes before Drain
// returns; nothing reports ready afterwards (the race found in review of FIN-275).
func TestUnitDrainWaitsForInFlightReadyNotification(t *testing.T) {
	r := &Readiness{}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	events := drainDuringNotification(t, r, slog.New(slog.DiscardHandler), entered, release, func(bool) {
		once.Do(func() { close(entered); <-release })
	})
	if len(events) != 2 || events[0] != "ready" || events[1] != "drained" || r.Ready() {
		t.Fatalf("events %v ready=%v, want [ready drained] and not ready", events, r.Ready())
	}
}

// blockingHandler holds the first "ready" log record until release is closed.
type blockingHandler struct {
	slog.Handler
	entered, release chan struct{}
	once             *sync.Once
}

func (blockingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h blockingHandler) Handle(ctx context.Context, rec slog.Record) error {
	if rec.Message == "ready" {
		h.once.Do(func() { close(h.entered); <-h.release })
	}
	return nil
}

// Review repro of FIN-275: Watch logs "ready" and is held there, Drain is called, then
// the log is released. The ready notification must not be delivered after Drain returned.
func TestUnitDrainDuringReadyLogDeliversNoLateReady(t *testing.T) {
	r := &Readiness{}
	h := blockingHandler{Handler: slog.DiscardHandler, entered: make(chan struct{}), release: make(chan struct{}), once: &sync.Once{}}
	events := drainDuringNotification(t, r, slog.New(h), h.entered, h.release, func(bool) {})
	if len(events) != 2 || events[0] != "ready" || events[1] != "drained" || r.Ready() {
		t.Fatalf("events %v ready=%v: a ready notification arrived after Drain returned", events, r.Ready())
	}
}
