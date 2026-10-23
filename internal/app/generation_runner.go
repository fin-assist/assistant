package app

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/fin-assist/assistant/internal/account"
	"github.com/fin-assist/assistant/internal/domain"
	"github.com/fin-assist/assistant/internal/generation"
)

const generationDeadline = 3 * time.Minute

type generationExecutor interface {
	Run(context.Context, domain.GenerationTask) error
}

type userNotifications interface {
	SubscribeUser(context.Context, string) (<-chan struct{}, func(), error)
}

// generationRunner owns tasks after acceptance, independently of unary/stream clients.
// Its close lock prevents a new WaitGroup.Add racing the shutdown Wait.
type generationRunner struct {
	ctx           context.Context
	cancel        context.CancelFunc
	engine        generationExecutor
	repo          generation.Repository
	tx            generation.Transactions
	tomb          generation.Tombstones
	now           func() time.Time
	log           *slog.Logger
	metrics       *generationMetrics
	notifications userNotifications
	mu            sync.Mutex
	closed        bool
	active        map[string]map[string]context.CancelFunc
	wg            sync.WaitGroup
}

func newGenerationRunner(ctx context.Context, engine generationExecutor, repo generation.Repository, tx generation.Transactions, tomb generation.Tombstones, now func() time.Time, log *slog.Logger) *generationRunner {
	ctx, cancel := context.WithCancel(ctx)
	return &generationRunner{ctx: ctx, cancel: cancel, engine: engine, repo: repo, tx: tx, tomb: tomb, now: now, log: log, active: make(map[string]map[string]context.CancelFunc)}
}

func (r *generationRunner) Ready() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.engine != nil && !r.closed && r.ctx.Err() == nil
}

func (r *generationRunner) Start(task domain.GenerationTask) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.ctx.Err() != nil || r.engine == nil {
		return
	}
	if _, exists := r.active[task.UserID][task.AnswerID]; exists {
		return
	}
	elapsed := max(time.Duration(0), r.now().Sub(task.CreatedAt))
	ctx, cancel := context.WithTimeout(r.ctx, max(time.Duration(0), generationDeadline-elapsed))
	if r.active[task.UserID] == nil {
		r.active[task.UserID] = make(map[string]context.CancelFunc)
	}
	r.active[task.UserID][task.AnswerID] = cancel
	if r.metrics != nil {
		r.metrics.accepted.Inc()
	}
	r.wg.Add(1)
	go r.run(ctx, cancel, task)
}

func (r *generationRunner) run(ctx context.Context, cancel context.CancelFunc, task domain.GenerationTask) {
	defer r.wg.Done()
	defer cancel()
	defer func() {
		r.mu.Lock()
		delete(r.active[task.UserID], task.AnswerID)
		if len(r.active[task.UserID]) == 0 {
			delete(r.active, task.UserID)
		}
		r.mu.Unlock()
	}()
	defer func() {
		if recover() != nil {
			r.fail(task)
		}
	}()
	if r.notifications != nil {
		changes, unsubscribe, err := r.notifications.SubscribeUser(ctx, task.UserID)
		if err != nil {
			r.fail(task)
			return
		}
		watchDone := make(chan struct{})
		go func() {
			defer close(watchDone)
			select {
			case <-ctx.Done():
			case <-changes:
				cancel()
			}
		}()
		defer func() { cancel(); unsubscribe(); <-watchDone }()
	}
	// Subscribe before the primary guard: a task starting after a purge cannot
	// send its saved question to the model, even if Start raced the deleting pod.
	err := r.tx.Run(ctx, func(ctx context.Context) error {
		alive, err := r.tomb.Guard(ctx, task.UserID)
		if err != nil {
			return err
		}
		if !alive {
			return generation.ErrStopped
		}
		return nil
	})
	if errors.Is(err, generation.ErrStopped) {
		return
	}
	if err != nil {
		r.fail(task)
		return
	}
	if err := r.engine.Run(ctx, task); err != nil && !errors.Is(err, generation.ErrStopped) {
		r.fail(task)
	}
}

func (r *generationRunner) fail(task domain.GenerationTask) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.finishFailed(ctx, task); err != nil {
		// Neither panic values nor dependency errors may expose dialogue or keys.
		r.log.ErrorContext(ctx, "answer finalization failed; stale-answer job will retry")
	}
}

func (r *generationRunner) finishFailed(ctx context.Context, task domain.GenerationTask) error {
	var changed bool
	err := r.tx.Run(ctx, func(ctx context.Context) error {
		alive, err := r.tomb.Guard(ctx, task.UserID)
		if err != nil || !alive {
			return err
		}
		changed, err = r.repo.Finish(ctx, task, generation.Outcome{ErrorCode: "MODEL_ERROR"}, r.now())
		return err
	})
	if err == nil && changed && r.metrics != nil {
		r.metrics.Finished("MODEL_ERROR", false, r.now().Sub(task.CreatedAt))
	}
	return err
}

func (r *generationRunner) Reap(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for {
		tasks, err := r.repo.Expired(ctx, r.now().Add(-generationDeadline), 100)
		if err != nil {
			return err
		}
		for _, task := range tasks {
			if err := r.finishFailed(ctx, task); err != nil {
				return err
			}
		}
		if len(tasks) < 100 {
			return nil
		}
	}
}

func (r *generationRunner) CancelUser(user string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, cancel := range r.active[user] {
		cancel()
	}
}

func (r *generationRunner) Close() {
	r.mu.Lock()
	r.closed = true
	r.cancel()
	r.mu.Unlock()
	r.wg.Wait()
}

type generationAccount struct {
	*account.Service
	runner *generationRunner
}

func (a generationAccount) Purge(ctx context.Context, user string) error {
	if err := a.Service.Purge(ctx, user); err != nil {
		return err
	}
	a.runner.CancelUser(user)
	return nil
}
