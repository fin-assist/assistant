package app_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	assistantv1 "github.com/fin-assist/contracts/gen/go/finassist/assistant/v1"
	deletionv1 "github.com/fin-assist/contracts/gen/go/finassist/userdeletion/v1"

	"github.com/fin-assist/assistant/internal/adapter/postgres"
	"github.com/fin-assist/assistant/internal/app"
	"github.com/fin-assist/assistant/internal/app/apitest"
	"github.com/fin-assist/assistant/internal/config"
	"github.com/fin-assist/assistant/internal/domain"
	"github.com/fin-assist/assistant/internal/generation"
)

var recoveryNow = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

const recoveryUser = "10000000-0000-0000-0000-000000000011"
const recoveryOther = "10000000-0000-0000-0000-000000000012"

func recoveryApp(t *testing.T) *apitest.Env {
	t.Helper()
	return apitest.StartWithOptions(t, app.Options{Clock: func() time.Time { return recoveryNow }})
}
func recoveryExec(t *testing.T, env *apitest.Env, query string, args ...any) {
	t.Helper()
	if _, err := env.App.DB.Writer(t.Context()).Exec(t.Context(), query, args...); err != nil {
		t.Fatal(err)
	}
}
func recoverySeed(t *testing.T, env *apitest.Env, user string, index int, created, chargeDay time.Time) domain.GenerationTask {
	t.Helper()
	task := domain.GenerationTask{UserID: user, QuestionID: fmt.Sprintf("20000000-0000-0000-0000-%012d", index), AnswerID: fmt.Sprintf("30000000-0000-0000-0000-%012d", index), Text: "committed question", TransferMode: "without", TimeZone: "Europe/Moscow", CreatedAt: created, QuestionCreatedAt: created}
	recoveryExec(t, env, `INSERT INTO question(user_id,id,text,transfer_mode,created_at) VALUES ($1,$2,$3,$4,$5)`, user, task.QuestionID, task.Text, task.TransferMode, created)
	recoveryExec(t, env, `INSERT INTO answer(user_id,id,question_id,charge_day,created_at) VALUES ($1,$2,$3,$4,$5)`, user, task.AnswerID, task.QuestionID, chargeDay.Format("2006-01-02"), created)
	recoveryExec(t, env, `INSERT INTO answer_block(user_id,answer_id,position,type,schema_version,markdown) VALUES ($1,$2,0,'text',1,'unfinished')`, user, task.AnswerID)
	return task
}
func recoveryUserState(t *testing.T, env *apitest.Env, user string, used int) {
	t.Helper()
	recoveryExec(t, env, `INSERT INTO user_state(user_id,limit_day,limit_used) VALUES ($1,$2,$3)`, user, recoveryNow.Format("2006-01-02"), used)
}
func recoveryJob(t *testing.T, a *app.App, name string) (func(), <-chan error) {
	t.Helper()
	for _, job := range a.Jobs {
		if job.Name != name {
			continue
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- job.Run(ctx); close(done) }()
		t.Cleanup(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("recovery job stopped with error: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Error("recovery job did not stop")
			}
		})
		return cancel, done
	}
	t.Fatalf("missing job %q", name)
	return nil, nil
}
func stopRecoveryJob(t *testing.T, cancel func(), done <-chan error) {
	t.Helper()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("job ignored cancellation")
	}
}
func recoveryObserver(t *testing.T, env *apitest.Env) *pgx.Conn {
	t.Helper()
	cfg := env.App.DB.Primary().Config().ConnConfig.Copy()
	cfg.RuntimeParams["application_name"] = "recovery-observer"
	conn, err := pgx.ConnectConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}
func recoveryCondition(t *testing.T, condition func(context.Context) (bool, error)) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		ok, err := condition(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("recovery condition timed out")
		case <-tick.C:
		}
	}
}
func recoveryWatch(t *testing.T, env *apitest.Env, task domain.GenerationTask) assistantv1.AssistantService_WatchAnswerClient {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	stream, err := assistantv1.NewAssistantServiceClient(env.Conn).WatchAnswer(apitest.User(ctx, task.UserID), &assistantv1.WatchAnswerRequest{AnswerId: task.AnswerID})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := stream.Recv()
	if err != nil || snapshot.GetSnapshot().GetStatus() != assistantv1.AnswerStatus_ANSWER_STATUS_GENERATING {
		t.Fatalf("committed task snapshot: %v %v", snapshot, err)
	}
	return stream
}
func recoveryFailure(t *testing.T, stream assistantv1.AssistantService_WatchAnswerClient, remaining int32) {
	t.Helper()
	event, err := stream.Recv()
	if err != nil || event.GetError().GetCode() != assistantv1.AnswerErrorCode_ANSWER_ERROR_CODE_MODEL_ERROR || !event.GetError().GetRetryable() || event.GetError().GetRemainingLimit() != remaining {
		t.Fatalf("recovered failure: %v %v", event, err)
	}
	if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("recovered stream EOF: %v", err)
	}
}
func recoveryCounter(t *testing.T, env *apitest.Env, user string) int {
	t.Helper()
	var used int
	if err := env.App.DB.Writer(t.Context()).QueryRow(t.Context(), `SELECT limit_used FROM user_state WHERE user_id=$1`, user).Scan(&used); err != nil {
		t.Fatal(err)
	}
	return used
}

func TestAPIGenerationRecoveryAPICommittedTaskWithoutStart(t *testing.T) {
	env := recoveryApp(t)
	recoveryUserState(t, env, recoveryUser, 1)
	// At the inclusive three-minute boundary this row represents a process dying
	// after acceptance committed and before its in-memory runner was started.
	task := recoverySeed(t, env, recoveryUser, 1, recoveryNow.Add(-3*time.Minute), recoveryNow)
	live := recoverySeed(t, env, recoveryUser, 2, recoveryNow.Add(-3*time.Minute+time.Microsecond), recoveryNow)
	recoveryExec(t, env, `UPDATE user_state SET limit_used=2 WHERE user_id=$1`, recoveryUser)
	stream := recoveryWatch(t, env, task)
	cancel, done := recoveryJob(t, env.App, "stale-generations")
	recoveryFailure(t, stream, 4)
	stopRecoveryJob(t, cancel, done)
	if got := recoveryCounter(t, env, recoveryUser); got != 1 {
		t.Fatalf("counter after recovery=%d, want1", got)
	}
	var savedStatus string
	var charged bool
	var finished time.Time
	if err := env.App.DB.Writer(t.Context()).QueryRow(t.Context(), `SELECT status,charged,finished_at FROM answer WHERE user_id=$1 AND id=$2`, task.UserID, task.AnswerID).Scan(&savedStatus, &charged, &finished); err != nil || savedStatus != "failed" || charged || !finished.Equal(recoveryNow) {
		t.Fatalf("durable recovery %q charged=%v finished=%v %v", savedStatus, charged, finished, err)
	}
	if err := env.App.DB.Writer(t.Context()).QueryRow(t.Context(), `SELECT status FROM answer WHERE user_id=$1 AND id=$2`, live.UserID, live.AnswerID).Scan(&savedStatus); err != nil || savedStatus != "generating" {
		t.Fatalf("not-yet-expired answer=%q %v", savedStatus, err)
	}
}

func TestAPIGenerationRecoveryAPITwoInstancesRefundExactlyOnceAndKeepToday(t *testing.T) {
	for _, pastDay := range []bool{false, true} {
		t.Run(map[bool]string{false: "current-day", true: "past-day"}[pastDay], func(t *testing.T) {
			env := recoveryApp(t)
			used := 1
			remaining := int32(5)
			chargeDay := recoveryNow
			if pastDay {
				used = 4
				remaining = 1
				chargeDay = recoveryNow.Add(-24 * time.Hour)
			}
			recoveryUserState(t, env, recoveryUser, used)
			task := recoverySeed(t, env, recoveryUser, 1, recoveryNow.Add(-4*time.Minute), chargeDay)
			replica, err := app.NewWithOptions(t.Context(), env.App.Cfg, slog.New(slog.DiscardHandler), prometheus.NewRegistry(), app.Options{Clock: func() time.Time { return recoveryNow }})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(replica.Close)
			stream := recoveryWatch(t, env, task)
			observer := recoveryObserver(t, env)
			blocker, err := observer.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = blocker.Rollback(context.Background()) }()
			if _, err := blocker.Exec(t.Context(), `SELECT 1 FROM user_state WHERE user_id=$1 FOR UPDATE`, recoveryUser); err != nil {
				t.Fatal(err)
			}
			cancelOne, doneOne := recoveryJob(t, env.App, "stale-generations")
			cancelTwo, doneTwo := recoveryJob(t, replica, "stale-generations")
			// Holding the user lock lets both pods select the same stale task. One blocks
			// on that row and the other on the shared tombstone guard, before either wins.
			var waitingPIDs []int32
			recoveryCondition(t, func(ctx context.Context) (bool, error) {
				if _, err := observer.Exec(ctx, `SELECT pg_stat_clear_snapshot()`); err != nil {
					return false, err
				}
				err := observer.QueryRow(ctx, `SELECT coalesce(array_agg(pid),'{}'::integer[]) FROM pg_stat_activity WHERE datname=current_database() AND application_name=$1 AND wait_event_type='Lock'`, env.App.Cfg.Service).Scan(&waitingPIDs)
				return len(waitingPIDs) >= 2, err
			})
			if err := blocker.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
			recoveryFailure(t, stream, remaining)
			// Observe both selected tasks finishing their transactions, not merely the
			// first stream event, before stopping either job.
			recoveryCondition(t, func(ctx context.Context) (bool, error) {
				var idle int
				err := observer.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE pid=ANY($1::integer[]) AND state='idle'`, waitingPIDs).Scan(&idle)
				return idle == len(waitingPIDs), err
			})
			stopRecoveryJob(t, cancelOne, doneOne)
			stopRecoveryJob(t, cancelTwo, doneTwo)
			want := 0
			if pastDay {
				want = 4
			}
			if got := recoveryCounter(t, env, recoveryUser); got != want {
				t.Fatalf("counter=%d want%d", got, want)
			}
		})
	}
}

func TestAPIGenerationRecoveryAPIBacklogAndCancellation(t *testing.T) {
	t.Run("multiple-batches", func(t *testing.T) {
		env := recoveryApp(t)
		recoveryUserState(t, env, recoveryUser, 101)
		recoveryExec(t, env, `INSERT INTO question(user_id,id,text,transfer_mode,created_at) SELECT $1,('20000000-0000-0000-0000-'||lpad(n::text,12,'0'))::uuid,'queued','without',$2 FROM generate_series(1,101)n`, recoveryUser, recoveryNow.Add(-4*time.Minute))
		recoveryExec(t, env, `INSERT INTO answer(user_id,id,question_id,charge_day,created_at) SELECT $1,('30000000-0000-0000-0000-'||lpad(n::text,12,'0'))::uuid,('20000000-0000-0000-0000-'||lpad(n::text,12,'0'))::uuid,$2,$3 FROM generate_series(1,101)n`, recoveryUser, recoveryNow.Format("2006-01-02"), recoveryNow.Add(-4*time.Minute))
		task := domain.GenerationTask{UserID: recoveryUser, AnswerID: "30000000-0000-0000-0000-000000000101"}
		stream := recoveryWatch(t, env, task)
		cancel, done := recoveryJob(t, env.App, "stale-generations")
		recoveryFailure(t, stream, 5)
		stopRecoveryJob(t, cancel, done)
		var terminal int
		if err := env.App.DB.Writer(t.Context()).QueryRow(t.Context(), `SELECT count(*) FROM answer WHERE user_id=$1 AND status='failed' AND charged=false`, recoveryUser).Scan(&terminal); err != nil || terminal != 101 || recoveryCounter(t, env, recoveryUser) != 0 {
			t.Fatalf("backlog terminal=%d %v", terminal, err)
		}
	})
	t.Run("cancel-blocked-recovery", func(t *testing.T) {
		env := recoveryApp(t)
		recoveryUserState(t, env, recoveryUser, 1)
		task := recoverySeed(t, env, recoveryUser, 1, recoveryNow.Add(-4*time.Minute), recoveryNow)
		observer := recoveryObserver(t, env)
		blocker, err := observer.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = blocker.Rollback(context.Background()) }()
		if _, err := blocker.Exec(t.Context(), `SELECT 1 FROM user_state WHERE user_id=$1 FOR UPDATE`, recoveryUser); err != nil {
			t.Fatal(err)
		}
		cancel, done := recoveryJob(t, env.App, "stale-generations")
		recoveryCondition(t, func(ctx context.Context) (bool, error) {
			if _, err := observer.Exec(ctx, `SELECT pg_stat_clear_snapshot()`); err != nil {
				return false, err
			}
			var count int
			err := observer.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND application_name=$1 AND wait_event_type='Lock'`, env.App.Cfg.Service).Scan(&count)
			return count >= 1, err
		})
		stopRecoveryJob(t, cancel, done)
		if err := blocker.Rollback(t.Context()); err != nil {
			t.Fatal(err)
		}
		var saved string
		if err := env.App.DB.Writer(t.Context()).QueryRow(t.Context(), `SELECT status FROM answer WHERE user_id=$1 AND id=$2`, task.UserID, task.AnswerID).Scan(&saved); err != nil || saved != "generating" || recoveryCounter(t, env, recoveryUser) != 1 {
			t.Fatalf("cancelled recovery mutated data: %q %v", saved, err)
		}
	})
}

func recoveryKey(t *testing.T, env *apitest.Env, user, endpoint string, index int, expires time.Time) {
	t.Helper()
	recoveryExec(t, env, `INSERT INTO idempotency_keys(scope,endpoint,key,request_hash,done,payload,expires_at) VALUES ($1,$2,$3,decode(repeat('01',32),'hex'),true,decode('01','hex'),$4)`, user, endpoint, fmt.Sprintf("40000000-0000-0000-0000-%012d", index), expires)
}
func TestAPIGenerationRecoveryAPIPurgeRemovesOnlyUsersDialogueAndKeys(t *testing.T) {
	env := recoveryApp(t)
	recoveryUserState(t, env, recoveryUser, 1)
	recoveryUserState(t, env, recoveryOther, 1)
	task := recoverySeed(t, env, recoveryUser, 1, recoveryNow, recoveryNow)
	recoverySeed(t, env, recoveryOther, 1, recoveryNow, recoveryNow)
	for _, user := range []string{recoveryUser, recoveryOther} {
		for _, endpoint := range []string{assistantv1.AssistantService_AskQuestion_FullMethodName, assistantv1.AssistantService_RetryAnswer_FullMethodName} {
			recoveryKey(t, env, user, endpoint, 1, recoveryNow.Add(time.Hour))
		}
	}
	stream := recoveryWatch(t, env, task)
	deletion := deletionv1.NewUserDeletionServiceClient(env.Conn)
	for range 2 {
		if _, err := deletion.PurgeUserData(apitest.Caller(t.Context(), "user"), &deletionv1.PurgeUserDataRequest{UserId: recoveryUser}); err != nil {
			t.Fatal(err)
		}
	}
	if event, err := stream.Recv(); event != nil || status.Code(err) != codes.NotFound || apitest.Reason(err) != "NOT_FOUND" {
		t.Fatalf("active deleted answer: %v %v", event, err)
	}
	for _, table := range []string{"user_state", "question", "answer", "answer_block", "idempotency_keys"} {
		column := "user_id"
		expectedOther := 1
		if table == "idempotency_keys" {
			column = "scope"
			expectedOther = 2
		}
		// Table/column names are constants from this test, never caller input.
		var own, other int
		query := fmt.Sprintf(`SELECT count(*) FILTER(WHERE %s=$1),count(*) FILTER(WHERE %s=$2) FROM %s`, column, column, table)
		if err := env.App.DB.Writer(t.Context()).QueryRow(t.Context(), query, recoveryUser, recoveryOther).Scan(&own, &other); err != nil || own != 0 || other != expectedOther {
			t.Fatalf("purge %s own=%d other=%d wantOther=%d: %v", table, own, other, expectedOther, err)
		}
	}
	err := env.App.DB.Run(t.Context(), func(ctx context.Context) error {
		return postgres.NewGenerations(env.App.DB).Append(ctx, task, 0, "late model output")
	})
	if !errors.Is(err, generation.ErrStopped) {
		t.Fatalf("late append after deletion: %v", err)
	}
	_, err = assistantv1.NewAssistantServiceClient(env.Conn).ListMessages(apitest.User(t.Context(), recoveryUser), &assistantv1.ListMessagesRequest{})
	if status.Code(err) != codes.Unauthenticated || apitest.Reason(err) != "TOKEN_REVOKED" {
		t.Fatalf("deleted account admission: %v", err)
	}
	var roots int
	if err := env.App.DB.Writer(t.Context()).QueryRow(t.Context(), `SELECT count(*) FROM user_state WHERE user_id=$1`, recoveryUser).Scan(&roots); err != nil || roots != 0 {
		t.Fatalf("late write resurrected root: %d %v", roots, err)
	}
}

func TestIntegrationGenerationRecoveryAPIIdempotencyCleanupKeepsLiveAndLockedKeys(t *testing.T) {
	env := recoveryApp(t)
	recoveryExec(t, env, `INSERT INTO idempotency_keys(scope,endpoint,key,request_hash,expires_at) SELECT $1,'expired',('40000000-0000-0000-0000-'||lpad(n::text,12,'0'))::uuid,decode(repeat('01',32),'hex'),$2 FROM generate_series(1,1001)n`, recoveryUser, recoveryNow)
	recoveryKey(t, env, recoveryUser, "live", 2000, recoveryNow.Add(time.Microsecond))
	recoveryKey(t, env, recoveryOther, "live", 2000, recoveryNow.Add(time.Hour))
	recoveryKey(t, env, recoveryUser, "locked", 2001, recoveryNow.Add(-time.Minute))
	observer := recoveryObserver(t, env)
	lock, err := observer.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Rollback(context.Background()) }()
	if _, err := lock.Exec(t.Context(), `SELECT 1 FROM idempotency_keys WHERE scope=$1 AND endpoint='locked' FOR UPDATE`, recoveryUser); err != nil {
		t.Fatal(err)
	}
	cancel, done := recoveryJob(t, env.App, "idempotency-cleanup")
	recoveryCondition(t, func(ctx context.Context) (bool, error) {
		var count int
		err := observer.QueryRow(ctx, `SELECT count(*) FROM idempotency_keys WHERE endpoint='expired'`).Scan(&count)
		return count == 0, err
	})
	stopRecoveryJob(t, cancel, done)
	var live, locked int
	if err := observer.QueryRow(t.Context(), `SELECT count(*) FILTER(WHERE endpoint='live'),count(*) FILTER(WHERE endpoint='locked') FROM idempotency_keys`).Scan(&live, &locked); err != nil || live != 2 || locked != 1 {
		t.Fatalf("cleanup retained live=%d locked=%d: %v", live, locked, err)
	}
	if err := lock.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	// A subsequent run can clean the now-unlocked expired record.
	cancel, done = recoveryJob(t, env.App, "idempotency-cleanup")
	recoveryCondition(t, func(ctx context.Context) (bool, error) {
		var count int
		err := observer.QueryRow(ctx, `SELECT count(*) FROM idempotency_keys WHERE expires_at<=$1`, recoveryNow).Scan(&count)
		return count == 0, err
	})
	stopRecoveryJob(t, cancel, done)
}

func TestUnitGenerationRecoveryAPIPeerOverridesRejectedOutsideDev(t *testing.T) {
	for _, environment := range []config.Env{config.EnvStage, config.EnvProd} {
		t.Run(string(environment), func(t *testing.T) {
			cfg := config.Config{Env: environment, DB: config.DBConfig{PrimaryDSN: "must-not-be-read"}}
			a, err := app.NewWithOptions(t.Context(), cfg, slog.New(slog.DiscardHandler), prometheus.NewRegistry(), app.Options{Clock: func() time.Time { return recoveryNow }, PeerDialOptions: map[string][]grpc.DialOption{"user": {grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
				t.Error("dev peer dialer reached outside dev")
				return nil, errors.New("unexpected dial")
			})}}})
			if a != nil {
				a.Close()
				t.Fatal("non-dev override admitted")
			}
			if err == nil || !strings.Contains(err.Error(), "peer transport overrides require ENV=dev") {
				t.Fatalf("override was not rejected before database/identity setup: %v", err)
			}
		})
	}
}
