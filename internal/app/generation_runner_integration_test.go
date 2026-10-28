package app

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/fin-assist/go-migrate"

	"github.com/fin-assist/assistant/internal/account"
	"github.com/fin-assist/assistant/internal/adapter/client"
	"github.com/fin-assist/assistant/internal/adapter/postgres"
	"github.com/fin-assist/assistant/internal/config"
	"github.com/fin-assist/assistant/internal/domain"
	"github.com/fin-assist/assistant/internal/generation"
	"github.com/fin-assist/assistant/internal/testdb"
)

const runnerPGUser = "10000000-0000-4000-8000-000000000011"

var runnerPGNow = time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)

type runnerPGModel func(context.Context, generation.ModelRequest, func(generation.ModelDelta) error, func([]byte) error) (generation.ModelResult, error)

func (f runnerPGModel) NewSession() generation.ModelSession {
	return generation.ModelSessionFunc(f.Stream)
}

func (f runnerPGModel) Stream(ctx context.Context, req generation.ModelRequest, emit func(generation.ModelDelta) error, reserve func([]byte) error) (generation.ModelResult, error) {
	raw, err := json.Marshal(req)
	if err != nil {
		return generation.ModelResult{}, err
	}
	if err := reserve(raw); err != nil {
		return generation.ModelResult{}, err
	}
	return f(ctx, req, emit, reserve)
}

type runnerPGEnv struct {
	app     *App
	runner  *generationRunner
	task    domain.GenerationTask
	metrics *generationMetrics
}

func newRunnerPGEnv(t *testing.T, model generation.Model, age time.Duration) *runnerPGEnv {
	t.Helper()
	dsn := testdb.DSN(t)
	cfg := migrate.DefaultConfig(dsn)
	cfg.Logger = slog.New(slog.DiscardHandler)
	if err := migrate.Up(t.Context(), cfg, MigrationSources()); err != nil {
		t.Fatal(err)
	}
	a, err := NewWithOptions(t.Context(), config.Config{Service: "runner-integration", Env: config.EnvDev, Auth: config.AuthConfig{Mode: config.AuthInsecureDev}, DB: config.DBConfig{PrimaryDSN: dsn, MaxConns: 8}, GRPC: config.GRPCConfig{MaxRecvMsgSize: 4 << 20}, DailyMax: 5, OffTopic: config.DefaultOffTopicConfig(), Peers: map[string]string{}}, slog.New(slog.DiscardHandler), prometheus.NewRegistry(), Options{Clock: func() time.Time { return runnerPGNow }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	repo := postgres.NewGenerations(a.DB)
	data, err := client.NewFinancialData(nil, nil, client.FinancialLimits{ResultBytes: 32768, CollectionItems: 200})
	if err != nil {
		t.Fatal(err)
	}
	tools, err := generation.NewToolRegistry(generation.BuiltinTools(data)...)
	if err != nil {
		t.Fatal(err)
	}
	metrics, err := newGenerationMetrics(prometheus.NewRegistry(), tools)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := generation.NewWithTools(repo, a.DB, a.Tombstones, model, tools, generation.EngineLimits{Budget: generation.BudgetLimits{InputTokensPerStep: 24000, InputTokensTotal: 48000, HistoryTokens: 16000, RequestBytesPerStep: 65536, RequestBytesTotal: 131072, MaxSteps: 8, MaxFunctions: 16}, MaxOutputTokens: 4096, ParallelFunctions: 4, MaxSearchCalls: 3, Observer: metrics}, func() time.Time { return runnerPGNow })
	if err != nil {
		t.Fatal(err)
	}
	runner := newGenerationRunner(t.Context(), engine, repo, a.DB, a.Tombstones, func() time.Time { return runnerPGNow }, a.Log)
	runner.metrics = metrics
	notifications, err := postgres.NewAnswerNotifications(t.Context(), a.DB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(notifications.Close)
	runner.notifications = notifications
	t.Cleanup(runner.Close)
	questions := postgres.NewQuestions(a.DB)
	var task domain.GenerationTask
	if err := a.DB.Run(t.Context(), func(ctx context.Context) error {
		alive, err := a.Tombstones.Guard(ctx, runnerPGUser)
		if err != nil {
			return err
		}
		if !alive {
			return errors.New("fixture user deleted")
		}
		if _, err = questions.Lock(ctx, runnerPGUser); err != nil {
			return err
		}
		if err = questions.Charge(ctx, runnerPGUser, runnerPGNow, 1); err != nil {
			return err
		}
		task, err = questions.Create(ctx, runnerPGUser, "saved financial question", "without", runnerPGNow.Add(-age))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return &runnerPGEnv{a, runner, task, metrics}
}
func (e *runnerPGEnv) assertFailed(t *testing.T, code string) {
	t.Helper()
	var status, gotCode string
	var charged bool
	var used, blocks int
	err := e.app.DB.Reader(t.Context()).QueryRow(t.Context(), `SELECT a.status,a.error_code,a.charged,u.limit_used,(SELECT count(*) FROM answer_block b WHERE b.user_id=a.user_id AND b.answer_id=a.id) FROM answer a JOIN user_state u USING(user_id) WHERE a.user_id=$1 AND a.id=$2`, runnerPGUser, e.task.AnswerID).Scan(&status, &gotCode, &charged, &used, &blocks)
	if err != nil || status != "failed" || gotCode != code || charged || used != 0 || blocks != 0 {
		t.Fatalf("durable terminal=%s/%s charged=%v used=%d blocks=%d err=%v", status, gotCode, charged, used, blocks, err)
	}
	if got := testutil.ToFloat64(e.metrics.refunded); got != 1 {
		t.Fatalf("refund metric=%v want1", got)
	}
}
func TestIntegrationGenerationRunnerOwnsDurableTerminalAfterCallerReturns(t *testing.T) {
	for _, scenario := range []string{"provider-error", "panic", "off-topic"} {
		t.Run(scenario, func(t *testing.T) {
			var calls atomic.Int32
			entered := make(chan struct{})
			release := make(chan struct{})
			model := runnerPGModel(func(ctx context.Context, _ generation.ModelRequest, _ func(generation.ModelDelta) error, _ func([]byte) error) (generation.ModelResult, error) {
				calls.Add(1)
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
					return generation.ModelResult{}, ctx.Err()
				}
				switch scenario {
				case "panic":
					panic("PRIVATE provider data")
				case "off-topic":
					return generation.ModelResult{Output: []generation.ModelItem{{ID: "call", Type: "function_call", CallID: "call", Name: "off_topic", Arguments: `{}`, Raw: []byte(`{"id":"call","type":"function_call","call_id":"call","name":"off_topic","arguments":"{}","status":"completed"}`)}}}, nil
				default:
					return generation.ModelResult{}, errors.New("PRIVATE provider error")
				}
			})
			e := newRunnerPGEnv(t, model, 0)
			if !e.runner.Ready() {
				t.Fatal("configured runner not ready")
			}
			e.runner.Start(e.task)
			runnerWait(t, entered)
			e.runner.Start(e.task)
			close(release)
			e.runner.wg.Wait()
			e.runner.Close()
			if calls.Load() != 1 {
				t.Fatalf("duplicate task executed%d times", calls.Load())
			}
			code := "MODEL_ERROR"
			if scenario == "off-topic" {
				code = "OFF_TOPIC"
			}
			e.assertFailed(t, code)
			if err := e.runner.Reap(t.Context()); err != nil {
				t.Fatal(err)
			}
			e.assertFailed(t, code)
			if e.runner.Ready() {
				t.Fatal("closed runner still ready")
			}
			e.runner.Start(e.task)
			if calls.Load() != 1 {
				t.Fatal("closed runner restarted a task")
			}
		})
	}
}
func TestIntegrationGenerationRunnerReapsCommittedExpiredAttemptOnce(t *testing.T) {
	var calls atomic.Int32
	e := newRunnerPGEnv(t, runnerPGModel(func(context.Context, generation.ModelRequest, func(generation.ModelDelta) error, func([]byte) error) (generation.ModelResult, error) {
		calls.Add(1)
		return generation.ModelResult{}, errors.New("must not execute")
	}), generationDeadline+time.Second)
	if err := e.runner.Reap(t.Context()); err != nil {
		t.Fatal(err)
	}
	e.assertFailed(t, "MODEL_ERROR")
	if err := e.runner.Reap(t.Context()); err != nil {
		t.Fatal(err)
	}
	e.assertFailed(t, "MODEL_ERROR")
	if calls.Load() != 0 {
		t.Fatal("reaper called model")
	}
}
func TestIntegrationGenerationRunnerCancellationAndRemotePurge(t *testing.T) {
	for _, scenario := range []string{"cancel-user", "remote-purge"} {
		t.Run(scenario, func(t *testing.T) {
			entered := make(chan struct{})
			cancelled := make(chan struct{})
			model := runnerPGModel(func(ctx context.Context, _ generation.ModelRequest, emit func(generation.ModelDelta) error, _ func([]byte) error) (generation.ModelResult, error) {
				if err := emit(generation.ModelDelta{Text: "private partial"}); err != nil {
					return generation.ModelResult{}, err
				}
				close(entered)
				<-ctx.Done()
				close(cancelled)
				return generation.ModelResult{}, ctx.Err()
			})
			e := newRunnerPGEnv(t, model, 0)
			e.runner.Start(e.task)
			runnerWait(t, entered)
			if scenario == "cancel-user" {
				e.runner.CancelUser(runnerPGUser)
			} else {
				// A separate process has no access to this runner's active map; only the
				// committed PostgreSQL notification and tombstone can cancel its request.
				accounts := account.New(e.app.DB, postgres.NewAccounts(e.app.DB), e.app.Tombstones, func() time.Time { return runnerPGNow }, 5)
				if err := accounts.Purge(t.Context(), runnerPGUser); err != nil {
					t.Fatal(err)
				}
			}
			runnerWait(t, cancelled)
			e.runner.Close()
			if scenario == "cancel-user" {
				e.assertFailed(t, "MODEL_ERROR")
				return
			}
			var rows int
			if err := e.app.DB.Reader(t.Context()).QueryRow(t.Context(), `SELECT (SELECT count(*) FROM user_state WHERE user_id=$1)+(SELECT count(*) FROM question WHERE user_id=$1)+(SELECT count(*) FROM answer WHERE user_id=$1)`, runnerPGUser).Scan(&rows); err != nil || rows != 0 {
				t.Fatalf("purge resurrected%d rows: %v", rows, err)
			}
			deleted, err := e.app.Tombstones.Deleted(t.Context(), runnerPGUser)
			if err != nil || !deleted {
				t.Fatalf("tombstone=%v %v", deleted, err)
			}
		})
	}
}

func TestIntegrationGenerationRunnerCommitsOneChargedCompleteAnswer(t *testing.T) {
	model := runnerPGModel(func(_ context.Context, _ generation.ModelRequest, emit func(generation.ModelDelta) error, _ func([]byte) error) (generation.ModelResult, error) {
		text := "Сохранённый ответ о финансах"
		if err := emit(generation.ModelDelta{Text: text}); err != nil {
			return generation.ModelResult{}, err
		}
		raw, err := json.Marshal(map[string]any{"id": "message", "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": text}}})
		return generation.ModelResult{Output: []generation.ModelItem{{ID: "message", Type: "message", Raw: raw}}, Usage: generation.Usage{InputTokens: 30, OutputTokens: 5, TotalTokens: 35}}, err
	})
	e := newRunnerPGEnv(t, model, 0)
	e.runner.Start(e.task)
	e.runner.wg.Wait()
	e.runner.Close()
	var status, text string
	var charged bool
	var used int
	err := e.app.DB.Reader(t.Context()).QueryRow(t.Context(), `SELECT a.status,a.charged,u.limit_used,b.markdown FROM answer a JOIN user_state u USING(user_id) JOIN answer_block b ON b.user_id=a.user_id AND b.answer_id=a.id WHERE a.user_id=$1 AND a.id=$2`, runnerPGUser, e.task.AnswerID).Scan(&status, &charged, &used, &text)
	if err != nil || status != "complete" || !charged || used != 1 || text != "Сохранённый ответ о финансах" {
		t.Fatalf("complete=%s charged=%v used=%d text=%q err=%v", status, charged, used, text, err)
	}
	if got := testutil.ToFloat64(e.metrics.finished.WithLabelValues("complete", "true")); got != 1 {
		t.Fatalf("completion metric=%v", got)
	}
	if got := testutil.ToFloat64(e.metrics.refunded); got != 0 {
		t.Fatalf("charged answer refund=%v", got)
	}
	if err := e.runner.Reap(t.Context()); err != nil {
		t.Fatal(err)
	}
	var after int
	if err := e.app.DB.Reader(t.Context()).QueryRow(t.Context(), `SELECT limit_used FROM user_state WHERE user_id=$1`, runnerPGUser).Scan(&after); err != nil || after != 1 {
		t.Fatalf("reaper modified committed charge=%d err=%v", after, err)
	}
}
