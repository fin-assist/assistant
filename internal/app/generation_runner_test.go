package app

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fin-assist/assistant/internal/dialogue"
	"github.com/fin-assist/assistant/internal/domain"
	"github.com/fin-assist/assistant/internal/generation"
)

type runnerEngine func(context.Context, domain.GenerationTask) error

type runnerNotifications struct {
	changes      chan struct{}
	unsubscribed chan struct{}
	err          error
}

func (n runnerNotifications) SubscribeUser(context.Context, string) (<-chan struct{}, func(), error) {
	if n.err != nil {
		return nil, nil, n.err
	}
	return n.changes, func() { close(n.unsubscribed) }, nil
}

func (f runnerEngine) Run(ctx context.Context, task domain.GenerationTask) error { return f(ctx, task) }

type runnerTransactions struct{ err error }

func (f runnerTransactions) Run(ctx context.Context, fn func(context.Context) error) error {
	if f.err != nil {
		return f.err
	}
	return fn(ctx)
}

type runnerTombstone struct {
	alive bool
	err   error
}

func (f runnerTombstone) Guard(context.Context, string) (bool, error) { return f.alive, f.err }

type runnerRepository struct {
	finish  func(context.Context, domain.GenerationTask, generation.Outcome, time.Time) (bool, error)
	expired func(context.Context, time.Time, int) ([]domain.GenerationTask, error)
}

func (*runnerRepository) History(context.Context, domain.GenerationTask, int) ([]generation.HistoryPair, error) {
	return nil, nil
}
func (*runnerRepository) Append(context.Context, domain.GenerationTask, int, string) error {
	return nil
}
func (*runnerRepository) AppendBlock(context.Context, domain.GenerationTask, int, dialogue.Block) error {
	return nil
}
func (r *runnerRepository) Finish(ctx context.Context, t domain.GenerationTask, o generation.Outcome, n time.Time) (bool, error) {
	if r.finish != nil {
		return r.finish(ctx, t, o, n)
	}
	return true, nil
}
func (r *runnerRepository) Expired(ctx context.Context, b time.Time, l int) ([]domain.GenerationTask, error) {
	if r.expired != nil {
		return r.expired(ctx, b, l)
	}
	return nil, nil
}
func runnerWait(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("runner did not reach the expected state")
	}
}
func testRunner(engine generationExecutor, repo *runnerRepository, now time.Time) *generationRunner {
	return newGenerationRunner(context.Background(), engine, repo, runnerTransactions{}, runnerTombstone{alive: true}, func() time.Time { return now }, slog.New(slog.DiscardHandler))
}

func TestGenerationRunnerFailureAndPanic(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	for _, cause := range []string{"error", "panic", "stopped"} {
		t.Run(cause, func(t *testing.T) {
			var finished atomic.Int32
			done := make(chan struct{})
			repo := &runnerRepository{finish: func(ctx context.Context, task domain.GenerationTask, out generation.Outcome, at time.Time) (bool, error) {
				if ctx.Err() != nil || out.ErrorCode != "MODEL_ERROR" || out.Charged || !at.Equal(now) {
					t.Error("invalid fallback finalization")
				}
				finished.Add(1)
				return true, nil
			}}
			r := testRunner(runnerEngine(func(context.Context, domain.GenerationTask) error {
				defer close(done)
				switch cause {
				case "panic":
					panic("private provider data")
				case "stopped":
					return generation.ErrStopped
				default:
					return errors.New("failure")
				}
			}), repo, now)
			t.Cleanup(r.Close)
			r.Start(domain.GenerationTask{UserID: "u", AnswerID: "a", CreatedAt: now})
			runnerWait(t, done)
			r.Close()
			want := int32(1)
			if cause == "stopped" {
				want = 0
			}
			if finished.Load() != want {
				t.Fatalf("terminal writes=%d, want %d", finished.Load(), want)
			}
		})
	}
}

func TestGenerationRunnerAdmissionAndCancellation(t *testing.T) {
	now := time.Now()
	var calls atomic.Int32
	started := make(chan struct{}, 2)
	cancelled := make(chan string, 2)
	r := testRunner(runnerEngine(func(ctx context.Context, task domain.GenerationTask) error {
		calls.Add(1)
		started <- struct{}{}
		<-ctx.Done()
		cancelled <- task.UserID
		return generation.ErrStopped
	}), &runnerRepository{}, now)
	t.Cleanup(r.Close)
	if !r.Ready() {
		t.Fatal("configured runner not ready")
	}
	r.Start(domain.GenerationTask{UserID: "u", AnswerID: "a", CreatedAt: now})
	runnerWait(t, started)
	r.Start(domain.GenerationTask{UserID: "u", AnswerID: "a", CreatedAt: now})
	r.Start(domain.GenerationTask{UserID: "v", AnswerID: "b", CreatedAt: now})
	runnerWait(t, started)
	r.CancelUser("u")
	select {
	case user := <-cancelled:
		if user != "u" {
			t.Fatal("another user's task cancelled")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("purged user's task not cancelled")
	}
	select {
	case <-cancelled:
		t.Fatal("unrelated task cancelled")
	default:
	}
	r.Close()
	if r.Ready() {
		t.Fatal("closed runner admits new work")
	}
	r.Start(domain.GenerationTask{UserID: "w", AnswerID: "c", CreatedAt: now})
	if calls.Load() != 2 {
		t.Fatalf("engine starts=%d, want 2", calls.Load())
	}
	select {
	case user := <-cancelled:
		if user != "v" {
			t.Fatal("wrong shutdown task")
		}
	default:
		t.Fatal("shutdown did not cancel and drain")
	}
	if testRunner(nil, &runnerRepository{}, now).Ready() {
		t.Fatal("unconfigured runner is ready")
	}
}

func TestGenerationRunnerConcurrentClose(t *testing.T) {
	now := time.Now()
	r := testRunner(runnerEngine(func(ctx context.Context, _ domain.GenerationTask) error { <-ctx.Done(); return generation.ErrStopped }), &runnerRepository{}, now)
	var writers sync.WaitGroup
	for range 20 {
		writers.Go(func() { r.Start(domain.GenerationTask{UserID: "u", AnswerID: "a", CreatedAt: now}) })
	}
	r.Close()
	writers.Wait()
	r.Close()
}

func TestGenerationRunnerDeadline(t *testing.T) {
	now := time.Now()
	for _, age := range []time.Duration{-time.Hour, time.Minute, 4 * time.Minute} {
		t.Run(age.String(), func(t *testing.T) {
			seen := make(chan struct{})
			r := testRunner(runnerEngine(func(ctx context.Context, _ domain.GenerationTask) error {
				d, ok := ctx.Deadline()
				if !ok {
					t.Error("missing deadline")
				}
				remaining := time.Until(d)
				want := generationDeadline - max(time.Duration(0), age)
				if age > generationDeadline {
					if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
						t.Error("expired task received live context")
					}
				} else if remaining > want || remaining < want-time.Second {
					t.Errorf("remaining=%s want <=%s", remaining, want)
				}
				close(seen)
				return generation.ErrStopped
			}), &runnerRepository{}, now)
			t.Cleanup(r.Close)
			r.Start(domain.GenerationTask{UserID: "u", AnswerID: "a", CreatedAt: now.Add(-age)})
			runnerWait(t, seen)
			r.Close()
		})
	}
}

func TestGenerationRunnerReap(t *testing.T) {
	now := time.Now()
	calls, finished := 0, 0
	repo := &runnerRepository{
		expired: func(_ context.Context, before time.Time, limit int) ([]domain.GenerationTask, error) {
			if !before.Equal(now.Add(-generationDeadline)) || limit != 100 {
				t.Error("wrong stale-answer boundary")
			}
			calls++
			if calls == 1 {
				return make([]domain.GenerationTask, 100), nil
			}
			return []domain.GenerationTask{{UserID: "last"}}, nil
		},
		finish: func(_ context.Context, _ domain.GenerationTask, out generation.Outcome, _ time.Time) (bool, error) {
			finished++
			if out.ErrorCode != "MODEL_ERROR" {
				t.Error("wrong stale outcome")
			}
			return true, nil
		},
	}
	r := testRunner(nil, repo, now)
	defer r.Close()
	if err := r.Reap(t.Context()); err != nil || calls != 2 || finished != 101 {
		t.Fatalf("reap err=%v batches=%d writes=%d", err, calls, finished)
	}
	boom := errors.New("storage failure")
	repo.expired = func(context.Context, time.Time, int) ([]domain.GenerationTask, error) { return nil, boom }
	if !errors.Is(r.Reap(t.Context()), boom) {
		t.Fatal("expired read failure lost")
	}
	repo.expired = func(context.Context, time.Time, int) ([]domain.GenerationTask, error) {
		return []domain.GenerationTask{{UserID: "u"}}, nil
	}
	r.tx = runnerTransactions{err: boom}
	if !errors.Is(r.Reap(t.Context()), boom) {
		t.Fatal("transaction failure lost")
	}
	r.tx = runnerTransactions{}
	r.tomb = runnerTombstone{err: boom}
	if !errors.Is(r.Reap(t.Context()), boom) {
		t.Fatal("guard failure lost")
	}
	r.tomb = runnerTombstone{alive: false}
	if err := r.Reap(t.Context()); err != nil {
		t.Fatal(err)
	}
	r.tomb = runnerTombstone{alive: true}
	repo.finish = func(context.Context, domain.GenerationTask, generation.Outcome, time.Time) (bool, error) {
		return false, boom
	}
	if !errors.Is(r.Reap(t.Context()), boom) {
		t.Fatal("terminal write failure lost")
	}
	r.fail(domain.GenerationTask{}) // Failure is recorded without leaking or panicking.
}

func TestGenerationRunnerUserDeletionObservation(t *testing.T) {
	for _, kind := range []string{"deleted before start", "notification", "transport failure"} {
		t.Run(kind, func(t *testing.T) {
			now := time.Now()
			started := make(chan struct{})
			stopped := make(chan struct{})
			var calls, finished atomic.Int32
			n := runnerNotifications{changes: make(chan struct{}), unsubscribed: make(chan struct{})}
			if kind == "transport failure" {
				n.err = errors.New("notification transport unavailable")
			}
			repo := &runnerRepository{finish: func(context.Context, domain.GenerationTask, generation.Outcome, time.Time) (bool, error) {
				finished.Add(1)
				close(stopped)
				return true, nil
			}}
			r := testRunner(runnerEngine(func(ctx context.Context, _ domain.GenerationTask) error {
				calls.Add(1)
				close(started)
				<-ctx.Done()
				return generation.ErrStopped
			}), repo, now)
			r.notifications = n
			if kind == "deleted before start" {
				r.tomb = runnerTombstone{alive: false}
			}
			t.Cleanup(r.Close)
			r.Start(domain.GenerationTask{UserID: "u", AnswerID: "a", CreatedAt: now})
			switch kind {
			case "notification":
				runnerWait(t, started)
				close(n.changes)
				runnerWait(t, n.unsubscribed)
			case "deleted before start":
				runnerWait(t, n.unsubscribed)
			case "transport failure":
				runnerWait(t, stopped)
			}
			r.Close()
			wantCalls, wantFinished := int32(0), int32(0)
			if kind == "notification" {
				wantCalls = 1
			}
			if kind == "transport failure" {
				wantFinished = 1
			}
			if calls.Load() != wantCalls || finished.Load() != wantFinished {
				t.Fatalf("calls=%d terminal=%d want %d/%d", calls.Load(), finished.Load(), wantCalls, wantFinished)
			}
		})
	}
}
