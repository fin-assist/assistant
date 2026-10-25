package asking

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/fin-assist/contracts/gen/go/finerr"
	idem "github.com/fin-assist/go-idempotency"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/fin-assist/assistant/internal/account"
	"github.com/fin-assist/assistant/internal/domain"
)

const user = "00000000-0000-4000-8000-000000000001"

var testNow = time.Date(2026, 10, 23, 22, 30, 0, 0, time.UTC)
var errDependency = errors.New("private dependency details")

func request() idem.Request {
	return idem.Request{Scope: user, Endpoint: "ask", Key: "00000000-0000-4000-8000-000000000002", Hash: [32]byte{1}}
}
func task() domain.GenerationTask {
	return domain.GenerationTask{UserID: user, QuestionID: "question", AnswerID: "answer", Text: "saved question", TransferMode: "without", TimeZone: "UTC", QuestionCreatedAt: testNow, CreatedAt: testNow}
}
func body(t *testing.T, reply Reply) []byte {
	t.Helper()
	b, err := json.Marshal(reply)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The opaque library reservation has no public constructor. Mint one through its
// public Reserve API; this stub only supplies a fresh key, never models our SQL.
type tokenDB struct{}

func (tokenDB) Tx(context.Context) (pgx.Tx, bool) { return tokenTx{}, true }

type tokenTx struct{ pgx.Tx }

func (tokenTx) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	return tokenRow{strings.HasPrefix(sql, "SELECT current_setting")}
}
func (tokenTx) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.NewCommandTag("SELECT 1"), nil
}

type tokenRow struct{ setting bool }

func (r tokenRow) Scan(dst ...any) error {
	if r.setting {
		*dst[0].(*string) = "0"
	} else {
		*dst[0].(*bool) = true
	}
	return nil
}
func reservation(t *testing.T) idem.Reservation {
	t.Helper()
	e, err := idem.New(tokenDB{}, idem.Options{Methods: map[string]idem.Policy{"ask": {}}, Clock: func() time.Time { return testNow }, Registerer: prometheus.NewRegistry()})
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := e.Reserve(t.Context(), request())
	if err != nil {
		t.Fatal(err)
	}
	return token
}

type fakeTX struct {
	snapshot                  func() func()
	events                    *[]string
	active                    bool
	calls, commits, rollbacks int
	begin, commit             map[int]error
}

func (f *fakeTX) Run(ctx context.Context, fn func(context.Context) error) error {
	f.calls++
	*f.events = append(*f.events, "begin")
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := f.begin[f.calls]; err != nil {
		return err
	}
	restore := func() {}
	if f.snapshot != nil {
		restore = f.snapshot()
	}
	f.active = true
	err := fn(ctx)
	f.active = false
	if err == nil {
		err = f.commit[f.calls]
	}
	if err != nil {
		restore()
		f.rollbacks++
		*f.events = append(*f.events, "rollback")
		return err
	}
	f.commits++
	*f.events = append(*f.events, "commit")
	return nil
}

type fakeGuard struct {
	events *[]string
	calls  int
	deadAt int
	errAt  int
}

func (f *fakeGuard) Guard(context.Context, string) (bool, error) {
	f.calls++
	*f.events = append(*f.events, "guard")
	if f.calls == f.errAt {
		return false, errDependency
	}
	return f.calls != f.deadAt, nil
}

type fakeRepo struct {
	createHook                              func(context.Context)
	events                                  *[]string
	state                                   account.State
	lockErr, createErr, retryErr, chargeErr error
	created                                 domain.GenerationTask
	day                                     time.Time
	used                                    int
	nowAtLock                               func()
	text, mode, answer                      string
	rateCompleted                           []time.Time
	rateErr                                 error
	rateCalls                               int
	rateUser                                string
	rateAfter                               time.Time
	rateLatest                              int
	rateContext                             context.Context
	rateHook                                func(context.Context)
	existsErr                               error
	existsCalls                             int
}

func (f *fakeRepo) Lock(context.Context, string) (account.State, error) {
	*f.events = append(*f.events, "lock")
	if f.nowAtLock != nil {
		f.nowAtLock()
	}
	return f.state, f.lockErr
}
func (f *fakeRepo) RecentOffTopic(ctx context.Context, user string, after time.Time, latest int) ([]time.Time, error) {
	*f.events = append(*f.events, "off-topic")
	f.rateCalls++
	f.rateUser, f.rateAfter, f.rateLatest, f.rateContext = user, after, latest, ctx
	if f.rateHook != nil {
		f.rateHook(ctx)
	}
	return f.rateCompleted, f.rateErr
}
func (f *fakeRepo) AnswerExists(context.Context, string, string) error {
	*f.events = append(*f.events, "exists")
	f.existsCalls++
	return f.existsErr
}
func (f *fakeRepo) Charge(_ context.Context, _ string, day time.Time, used int) error {
	*f.events = append(*f.events, "charge")
	f.day = day
	f.used = used
	return f.chargeErr
}
func (f *fakeRepo) Create(ctx context.Context, _ string, text, mode string, at time.Time) (domain.GenerationTask, error) {
	*f.events = append(*f.events, "create")
	if f.createHook != nil {
		f.createHook(ctx)
	}
	f.text = text
	f.mode = mode
	f.created.CreatedAt = at
	return f.created, f.createErr
}
func (f *fakeRepo) Retry(_ context.Context, _ string, answer string, at time.Time) (domain.GenerationTask, error) {
	*f.events = append(*f.events, "retry")
	f.answer = answer
	f.created.CreatedAt = at
	return f.created, f.retryErr
}

type fakeKeys struct {
	original                            context.Context
	events                              *[]string
	token                               idem.Reservation
	reserve, complete                   idem.Result
	reserveErr, completeErr, releaseErr error
	skipEffect                          bool
	releases                            int
	releasedContext                     context.Context
	completionContext                   context.Context
	callbackError                       error
	requests                            []idem.Request
}

func (f *fakeKeys) Reserve(_ context.Context, request idem.Request) (idem.Reservation, idem.Result, error) {
	*f.events = append(*f.events, "reserve")
	f.requests = append(f.requests, request)
	return f.token, f.reserve, f.reserveErr
}
func (f *fakeKeys) WithCompletion(ctx context.Context) (context.Context, context.CancelFunc) {
	*f.events = append(*f.events, "completion")
	f.original = ctx
	completion, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	f.completionContext = completion
	return f.completionContext, cancel
}
func (f *fakeKeys) DoReserved(ctx context.Context, _ idem.Reservation, fn func(context.Context) (idem.Response, error)) (idem.Result, error) {
	*f.events = append(*f.events, "finalize")
	if f.original.Err() != nil {
		return idem.Result{}, f.original.Err()
	}
	if f.skipEffect {
		return f.complete, f.completeErr
	}
	response, err := fn(ctx)
	f.callbackError = err
	if err != nil {
		return idem.Result{Err: finerr.RetryNotAllowed()}, nil //nolint:nilerr // the library may store a callback refusal and let the service commit
	}
	return idem.Result{Response: response}, f.completeErr
}
func (f *fakeKeys) Release(ctx context.Context, _ idem.Reservation) error {
	*f.events = append(*f.events, "release")
	f.releases++
	if ctx.Err() != nil {
		panic("release used canceled request")
	}
	if _, ok := ctx.Deadline(); !ok {
		panic("unbounded cleanup")
	}
	f.releasedContext = ctx
	return f.releaseErr
}

type fakeConsent struct {
	tx     *fakeTX
	events *[]string
	grant  bool
	err    error
	hook   func()
}

func (f *fakeConsent) Granted(context.Context, string) (bool, error) {
	if f.tx.active {
		panic("network inside transaction")
	}
	*f.events = append(*f.events, "consent")
	if f.hook != nil {
		f.hook()
	}
	return f.grant, f.err
}

type fakeStarter struct {
	tx      *fakeTX
	events  *[]string
	ready   bool
	started []domain.GenerationTask
}

func (f *fakeStarter) Ready() bool { *f.events = append(*f.events, "ready"); return f.ready }
func (f *fakeStarter) Start(task domain.GenerationTask) {
	if f.tx.active {
		panic("start before commit")
	}
	*f.events = append(*f.events, "start")
	f.started = append(f.started, task)
}

type setup struct {
	s       *Service
	tx      *fakeTX
	guard   *fakeGuard
	repo    *fakeRepo
	keys    *fakeKeys
	consent *fakeConsent
	starter *fakeStarter
	events  *[]string
}

func configured(t *testing.T) setup {
	t.Helper()
	events := []string{}
	tx := &fakeTX{events: &events}
	repo := &fakeRepo{events: &events, created: task()}
	tx.snapshot = func() func() { day, used := repo.day, repo.used; return func() { repo.day = day; repo.used = used } }
	guard := &fakeGuard{events: &events}
	keys := &fakeKeys{events: &events, token: reservation(t)}
	consent := &fakeConsent{events: &events, tx: tx, grant: true}
	starter := &fakeStarter{events: &events, tx: tx, ready: true}
	s := New(tx, repo, guard, keys, consent, starter, func() time.Time { return testNow }, 5, domain.OffTopicPolicy{Max: 5, Window: 7 * 24 * time.Hour})
	return setup{s, tx, guard, repo, keys, consent, starter, &events}
}

func TestUnitAcceptCommitOrderAndSavedReply(t *testing.T) {
	f := configured(t)
	got, err := f.s.Ask(t.Context(), user, request(), "question", "with")
	if err != nil || got.Remaining != 4 || got.Task.TransferMode != "without" || f.repo.text != "question" || f.repo.mode != "with" || f.repo.used != 1 || len(f.starter.started) != 1 {
		t.Fatalf("%+v %v %+v", got, err, f.repo)
	}
	want := []string{"begin", "guard", "reserve", "off-topic", "commit", "consent", "ready", "completion", "begin", "guard", "finalize", "lock", "off-topic", "charge", "create", "commit", "start"}
	if !reflect.DeepEqual(*f.events, want) {
		t.Fatalf("%v", *f.events)
	}
	if f.repo.day.Format("2006-01-02 -07:00") != "2026-10-24 +03:00" {
		t.Fatal(f.repo.day)
	}
	if f.keys.completionContext.Err() != context.Canceled {
		t.Fatal("completion deadline not released")
	}
}
func TestUnitRetryUsesStoredQuestionAndRollsBackChargeOnRefusal(t *testing.T) {
	f := configured(t)
	got, err := f.s.Retry(t.Context(), user, request(), "old answer")
	if err != nil || got.Task.Text != "saved question" || f.repo.answer != "old answer" || f.repo.text != "" || got.Remaining != 4 {
		t.Fatalf("%+v %v", got, err)
	}
	f = configured(t)
	f.repo.retryErr = ErrRetryDenied
	if _, err = f.s.Retry(t.Context(), user, request(), "old"); !errors.Is(err, ErrRetryDenied) || f.repo.used != 0 || f.tx.rollbacks != 1 || f.keys.releases != 1 || len(f.starter.started) != 0 {
		t.Fatalf("%v %+v", err, f)
	}
	// A transient refusal did not pin the key: a later failed answer may be retried.
	f.repo.retryErr = nil
	if _, err = f.s.Retry(t.Context(), user, request(), "old"); err != nil || len(f.starter.started) != 1 {
		t.Fatalf("%v", err)
	}
}
func TestUnitReplayBypassesConsentLimitsAndStarter(t *testing.T) {
	for _, stage := range []string{"reserve", "finalize"} {
		t.Run(stage, func(t *testing.T) {
			f := configured(t)
			r := idem.Result{Replayed: true, Response: idem.Response{Body: body(t, Reply{Task: task(), Remaining: 2})}}
			// A replay found at reservation precedes every new-attempt check. One
			// found only at finalization had already passed the open pre-consent gate.
			wantGateReads := 1
			if stage == "reserve" {
				f.repo.rateErr = errDependency
				f.keys.reserve = r
				f.keys.token = idem.Reservation{}
				f.consent.err = errDependency
				f.starter.ready = false
				f.repo.lockErr = errDependency
				wantGateReads = 0
			} else {
				f.keys.skipEffect = true
				f.keys.complete = r
			}
			got, err := f.s.Ask(t.Context(), user, request(), "new text", "with")
			if err != nil || got.Remaining != 2 || len(f.starter.started) != 0 || f.repo.used != 0 || f.keys.releases != 0 || f.repo.rateCalls != wantGateReads {
				t.Fatalf("%+v %v", got, err)
			}
			if stage == "reserve" && len(*f.events) != 4 {
				t.Fatal(*f.events)
			}
		})
	}
}
func TestUnitAcceptFailureBoundaries(t *testing.T) {
	cases := []struct {
		name           string
		change         func(*setup)
		want           error
		create, charge bool
	}{
		{"scope", func(f *setup) {}, ErrDeleted, false, false},
		{"reserve begin", func(f *setup) { f.tx.begin = map[int]error{1: errDependency} }, errDependency, false, false},
		{"guard first", func(f *setup) { f.guard.errAt = 1 }, errDependency, false, false},
		{"deleted first", func(f *setup) { f.guard.deadAt = 1 }, ErrDeleted, false, false},
		{"reserve error", func(f *setup) { f.keys.reserveErr = errDependency }, errDependency, false, false},
		{"reserve commit", func(f *setup) { f.tx.commit = map[int]error{1: errDependency} }, errDependency, false, false},
		{"off-topic pre-read", func(f *setup) { f.repo.rateErr = errDependency }, errDependency, false, false},
		{"consent unavailable", func(f *setup) { f.consent.err = errDependency }, ErrUnavailable, false, false},
		{"consent denied", func(f *setup) { f.consent.grant = false }, ErrConsent, false, false},
		{"not ready", func(f *setup) { f.starter.ready = false }, ErrUnavailable, false, false},
		{"no starter", func(f *setup) { f.s.starter = nil }, ErrUnavailable, false, false},
		{"effect begin", func(f *setup) { f.tx.begin = map[int]error{2: errDependency} }, errDependency, false, false},
		{"guard second", func(f *setup) { f.guard.errAt = 2 }, errDependency, false, false},
		{"deleted second", func(f *setup) { f.guard.deadAt = 2 }, ErrDeleted, false, false},
		{"fence", func(f *setup) { f.keys.skipEffect = true; f.keys.completeErr = errDependency }, errDependency, false, false},
		{"not stored", func(f *setup) {
			f.keys.skipEffect = true
			f.keys.completeErr = idem.ErrNotStored
			f.keys.complete.Err = ErrConsent
		}, ErrConsent, false, false},
		{"key error no result", func(f *setup) { f.keys.skipEffect = true; f.keys.completeErr = idem.ErrNotStored }, idem.ErrNotStored, false, false},
		{"lock", func(f *setup) { f.repo.lockErr = errDependency }, errDependency, false, false},
		{"off-topic locked read", func(f *setup) {
			f.repo.rateHook = func(context.Context) {
				if f.repo.rateCalls == 2 {
					f.repo.rateErr = errDependency
				}
			}
		}, errDependency, false, false},
		{"create", func(f *setup) { f.repo.createErr = errDependency }, errDependency, true, true},
		{"charge", func(f *setup) { f.repo.chargeErr = errDependency }, errDependency, false, true},
		{"effect commit", func(f *setup) { f.tx.commit = map[int]error{2: errDependency} }, errDependency, true, true},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			f := configured(t)
			tt.change(&f)
			key := request()
			if tt.name == "scope" {
				key.Scope = "foreign"
			}
			_, err := f.s.Ask(t.Context(), user, key, "question", "with")
			if !errors.Is(err, tt.want) || len(f.starter.started) != 0 {
				t.Fatalf("%v %v", err, *f.events)
			}
			events := strings.Join(*f.events, ",")
			if strings.Contains(events, "create") != tt.create || strings.Contains(events, "charge") != tt.charge {
				t.Fatal(*f.events)
			}
		})
	}
}
func TestUnitCounterUsesClockAfterLockAndNeverMovesBackwards(t *testing.T) {
	if (&LimitError{}).Error() != "assistant question limit exceeded" {
		t.Fatal("unstable domain error")
	}
	for _, tt := range []struct {
		name                string
		state               account.State
		daily               int
		wantDay             string
		wantUsed, remaining int
	}{
		{"midnight", account.State{LimitDay: time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC), Used: 5}, 5, "2026-10-24", 1, 4},
		{"clock rollback", account.State{LimitDay: time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC), Used: 1}, 5, "2026-10-25", 2, 3},
		{"decreased setting", account.State{LimitDay: time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC), Used: 5}, 2, "", 0, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := configured(t)
			f.repo.state = tt.state
			f.s.dailyMax = tt.daily
			locked := false
			f.repo.nowAtLock = func() { locked = true }
			f.s.now = func() time.Time {
				if !locked {
					// Only the unserialized pre-consent gate reads the clock here.
					// Charging from this instant would select the previous day.
					return testNow.Add(-24 * time.Hour)
				}
				return testNow
			}
			got, err := f.s.Ask(t.Context(), user, request(), "q", "with")
			if tt.wantDay == "" {
				var limit *LimitError
				if !errors.As(err, &limit) || limit.ResetsAt.Format("2006-01-02") != "2026-10-25" || f.repo.used != 0 {
					t.Fatalf("%v %+v", err, f.repo)
				}
				return
			}
			if err != nil || f.repo.day.Format("2006-01-02") != tt.wantDay || f.repo.used != tt.wantUsed || got.Remaining != tt.remaining {
				t.Fatalf("%+v %v %+v", got, err, f.repo)
			}
		})
	}
}
func TestUnitCancellationAndDetachedCleanup(t *testing.T) {
	f := configured(t)
	ctx, cancel := context.WithCancel(t.Context())
	f.consent.err = errDependency
	f.consent.hook = cancel
	if _, err := f.s.Ask(ctx, user, request(), "q", "with"); !errors.Is(err, context.Canceled) || f.keys.releases != 1 {
		t.Fatal(err)
	}
	if f.keys.releasedContext.Err() != context.Canceled {
		t.Fatal("cleanup context must be canceled after release")
	}
	f = configured(t)
	f.consent.grant = false
	f.keys.releaseErr = errDependency
	if _, err := f.s.Ask(t.Context(), user, request(), "q", "with"); err == nil || err.Error() != "assistant reservation cleanup failed" {
		t.Fatal(err)
	}
	f = configured(t)
	f.keys.reserveErr = errDependency
	f.keys.token = idem.Reservation{}
	if _, err := f.s.Ask(t.Context(), user, request(), "q", "with"); !errors.Is(err, errDependency) || f.keys.releases != 0 {
		t.Fatal(err)
	}
}
func TestUnitSavedOutcomeErrorsAndInvalidPayloads(t *testing.T) {
	for _, tt := range []struct {
		name string
		data []byte
		err  error
	}{
		{"cached error", nil, finerr.RetryNotAllowed()}, {"invalid json", []byte("broken"), nil},
		{"foreign task", body(t, Reply{Task: domain.GenerationTask{UserID: "foreign", QuestionID: "q", AnswerID: "a"}}), nil},
		{"missing question", body(t, Reply{Task: domain.GenerationTask{UserID: user, AnswerID: "a"}}), nil},
		{"missing answer", body(t, Reply{Task: domain.GenerationTask{UserID: user, QuestionID: "q"}}), nil},
		{"negative limit", body(t, Reply{Task: task(), Remaining: -1}), nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := configured(t)
			f.keys.reserve = idem.Result{Replayed: true, Response: idem.Response{Body: tt.data}, Err: tt.err}
			f.keys.token = idem.Reservation{}
			_, err := f.s.Ask(t.Context(), user, request(), "q", "with")
			if err == nil || f.tx.commits != 1 || f.keys.releases != 0 || len(f.starter.started) != 0 {
				t.Fatalf("%v %+v", err, f)
			}
		})
	}
	f := configured(t)
	f.repo.created.CreatedAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	f.s.now = func() time.Time { return f.repo.created.CreatedAt }
	if _, err := f.s.Ask(t.Context(), user, request(), "q", "with"); err == nil || f.tx.rollbacks != 1 || len(f.starter.started) != 0 {
		t.Fatal(err)
	}
}

func TestUnitDuplicateWhileOwnerReadsConsentDoesNotStartSecondGeneration(t *testing.T) {
	f := configured(t)
	entered, resume := make(chan struct{}), make(chan struct{})
	f.consent.hook = func() { close(entered); <-resume }
	owner := make(chan error, 1)
	go func() { _, err := f.s.Ask(t.Context(), user, request(), "q", "with"); owner <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("owner did not reach consent")
	}
	// The owner has committed its reservation, and holds no service transaction.
	// The key adapter now rejects the duplicate without returning another token.
	f.keys.reserveErr = finerr.RequestInProgress(time.Second)
	f.keys.token = idem.Reservation{}
	if _, err := f.s.Ask(t.Context(), user, request(), "q", "with"); err == nil || len(f.starter.started) != 0 {
		t.Fatal(err)
	}
	f.keys.reserveErr = nil
	close(resume)
	select {
	case err := <-owner:
		if err != nil || len(f.starter.started) != 1 || f.repo.used != 1 {
			t.Fatalf("%v %+v", err, f)
		}
	case <-time.After(time.Second):
		t.Fatal("owner stalled")
	}
}

func TestUnitOriginalCancellationBeforeEffectRollsBackButDuringEffectCommits(t *testing.T) {
	t.Run("before effect", func(t *testing.T) {
		f := configured(t)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		f.consent.hook = cancel
		if _, err := f.s.Ask(ctx, user, request(), "q", "with"); !errors.Is(err, context.Canceled) || f.repo.used != 0 || len(f.starter.started) != 0 || f.keys.releases != 1 {
			t.Fatalf("%v %v", err, *f.events)
		}
	})
	t.Run("during effect", func(t *testing.T) {
		f := configured(t)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		f.repo.createHook = func(work context.Context) {
			cancel()
			if work.Err() != nil {
				t.Fatal("effect did not receive detached completion context")
			}
		}
		if _, err := f.s.Ask(ctx, user, request(), "q", "with"); err != nil || f.repo.used != 1 || len(f.starter.started) != 1 || f.keys.releases != 0 || f.tx.commits != 2 {
			t.Fatalf("%v %v", err, *f.events)
		}
	})
}

func TestUnitRollingOffTopicGateRejectsAskAndRetryBeforeDailyCharge(t *testing.T) {
	for _, retry := range []bool{false, true} {
		t.Run(map[bool]string{false: "ask", true: "retry"}[retry], func(t *testing.T) {
			f := configured(t)
			// Both limits are exhausted: the rolling refusal must take precedence.
			f.repo.state = account.State{LimitDay: testNow, Used: 5}
			f.repo.rateCompleted = repeatedOffTopic(testNow.Add(-7*24*time.Hour + 3*time.Hour))
			var err error
			if retry {
				_, err = f.s.Retry(t.Context(), user, request(), "old answer")
			} else {
				_, err = f.s.Ask(t.Context(), user, request(), "question", "with")
			}
			var limited *RateLimitError
			if !errors.As(err, &limited) || !limited.RetryAt.Equal(testNow.Add(3*time.Hour)) || limited.Error() != "assistant off-topic limit exceeded" {
				t.Fatalf("rolling refusal lost: %v", err)
			}
			if f.repo.rateCalls != 1 || f.repo.rateUser != user || !f.repo.rateAfter.Equal(testNow.Add(-7*24*time.Hour)) || f.repo.rateLatest != 5 || f.repo.used != 0 || f.repo.text != "" || f.repo.answer != "" || len(f.starter.started) != 0 || f.keys.releases != 1 || f.tx.rollbacks != 1 || f.tx.commits != 1 {
				t.Fatalf("refusal created/charged/stored a result: %v %+v", *f.events, f.repo)
			}
			// grpc-api.md 7.4: 429 precedes the consent read (403) in user.
			want := []string{"begin", "guard", "reserve", "off-topic", "rollback", "begin", "release", "commit"}
			if !reflect.DeepEqual(*f.events, want) {
				t.Fatalf("gate did not precede consent: %v", *f.events)
			}
		})
	}
}

func TestUnitOffTopicRefusalReleasesSameKeyForExpiryOrSuccessfulReset(t *testing.T) {
	for _, retry := range []bool{false, true} {
		for _, cause := range []string{"expiry", "successful reset"} {
			t.Run(map[bool]string{false: "ask/", true: "retry/"}[retry]+cause, func(t *testing.T) {
				f := configured(t)
				f.repo.rateCompleted = repeatedOffTopic(testNow.Add(-7*24*time.Hour + time.Hour))
				accept := func() (Reply, error) {
					if retry {
						return f.s.Retry(t.Context(), user, request(), "old answer")
					}
					return f.s.Ask(t.Context(), user, request(), "question", "with")
				}
				for range 2 {
					_, err := accept()
					var limited *RateLimitError
					if !errors.As(err, &limited) {
						t.Fatal(err)
					}
				}
				if f.keys.releases != 2 || f.repo.used != 0 || len(f.starter.started) != 0 {
					t.Fatalf("repeated refusal had an effect: %v", *f.events)
				}
				// Expiry changes no facts; a charged-success reset clears the ledger
				// even before the previously advertised retry_at.
				if cause == "expiry" {
					now := testNow.Add(time.Hour)
					f.s.now = func() time.Time { return now }
				} else {
					f.repo.rateCompleted = nil
				}
				out, err := accept()
				if err != nil || out.Remaining != 4 || f.repo.used != 1 || len(f.starter.started) != 1 || f.keys.releases != 2 || f.repo.rateCalls != 4 {
					t.Fatalf("same key did not resume: %+v %v %v", out, err, *f.events)
				}
				if len(f.keys.requests) != 3 || f.keys.requests[0] != f.keys.requests[1] || f.keys.requests[1] != f.keys.requests[2] {
					t.Fatal("resume changed the idempotency identity")
				}
			})
		}
	}
}

func TestUnitOffTopicCheckUsesClockAfterLockAndCompletionContext(t *testing.T) {
	f := configured(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	locked := false
	acceptedAt := testNow.Add(2 * time.Hour)
	f.repo.nowAtLock = func() { locked = true }
	f.s.now = func() time.Time {
		if !locked {
			return testNow
		}
		return acceptedAt
	}
	f.repo.rateHook = func(work context.Context) {
		if !locked {
			if !f.tx.active || work != ctx {
				t.Fatal("pre-consent gate did not use the reservation transaction")
			}
			return
		}
		cancel()
		if !f.tx.active || work == ctx || work.Err() != nil {
			t.Fatal("weekly guard did not use the live effect transaction/context")
		}
		if _, ok := work.Deadline(); !ok {
			t.Fatal("weekly guard has no completion deadline")
		}
	}
	out, err := f.s.Ask(ctx, user, request(), "question", "with")
	if err != nil || !f.repo.rateAfter.Equal(acceptedAt.Add(-7*24*time.Hour)) || !out.Task.CreatedAt.Equal(acceptedAt) || f.repo.rateCalls != 2 || f.repo.used != 1 || len(f.starter.started) != 1 {
		t.Fatalf("clock/context split across guard and creation: %+v %v", out, err)
	}
}

func TestUnitOffTopicDependencyFailuresReleaseKeyAndDoNotCharge(t *testing.T) {
	for _, failure := range []error{errDependency, context.Canceled, context.DeadlineExceeded} {
		t.Run(failure.Error(), func(t *testing.T) {
			f := configured(t)
			f.repo.rateErr = failure
			if _, err := f.s.Ask(t.Context(), user, request(), "question", "with"); !errors.Is(err, failure) || f.keys.releases != 1 || f.repo.used != 0 || len(f.starter.started) != 0 || f.repo.text != "" {
				t.Fatalf("dependency failure lost or charged: %v %v", err, *f.events)
			}
		})
	}
}

// grpc-api.md 7.4 and 5.2: a blocked user gets 429 before the consent read in
// user (403/UNAVAILABLE) and before the readiness refusal; nothing is kept.
func TestUnitOffTopicGatePrecedesConsentAndReadiness(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*setup)
	}{
		{"consent missing", func(f *setup) { f.consent.grant = false }},
		{"consent unavailable", func(f *setup) { f.consent.err = errDependency }},
		{"generation unavailable", func(f *setup) { f.starter.ready = false }},
	} {
		for _, retry := range []bool{false, true} {
			t.Run(tc.name+map[bool]string{false: "/ask", true: "/retry"}[retry], func(t *testing.T) {
				f := configured(t)
				f.repo.rateCompleted = repeatedOffTopic(testNow.Add(-7*24*time.Hour + time.Hour))
				tc.change(&f)
				var err error
				if retry {
					_, err = f.s.Retry(t.Context(), user, request(), "old answer")
				} else {
					_, err = f.s.Ask(t.Context(), user, request(), "question", "with")
				}
				var limited *RateLimitError
				if !errors.As(err, &limited) || !limited.RetryAt.Equal(testNow.Add(time.Hour)) || f.repo.rateCalls != 1 || f.repo.used != 0 || f.keys.releases != 1 || len(f.starter.started) != 0 {
					t.Fatalf("weekly gate did not precede consent/readiness: %v %v", err, *f.events)
				}
				if strings.Contains(strings.Join(*f.events, ","), "consent") || strings.Contains(strings.Join(*f.events, ","), "ready") {
					t.Fatalf("blocked user reached consent or readiness: %v", *f.events)
				}
			})
		}
	}
}

// Without a block the existing consent/readiness refusals still apply and the
// locked re-check never runs.
func TestUnitOpenOffTopicGateKeepsConsentAndReadinessChecks(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*setup)
		want   error
	}{
		{"consent missing", func(f *setup) { f.consent.grant = false }, ErrConsent},
		{"consent unavailable", func(f *setup) { f.consent.err = errDependency }, ErrUnavailable},
		{"generation unavailable", func(f *setup) { f.starter.ready = false }, ErrUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := configured(t)
			f.repo.rateCompleted = repeatedOffTopic(testNow.Add(-time.Hour))[:4]
			tc.change(&f)
			if _, err := f.s.Ask(t.Context(), user, request(), "question", "with"); !errors.Is(err, tc.want) || f.repo.rateCalls != 1 || f.repo.used != 0 || f.keys.releases != 1 || len(f.starter.started) != 0 {
				t.Fatalf("existing refusal lost: %v %v", err, *f.events)
			}
		})
	}
}

// A fifth outcome committed after the pre-consent read must still refuse the
// attempt under the user lock, after consent was read once.
func TestUnitLockedOffTopicRecheckCatchesConcurrentFifthOutcome(t *testing.T) {
	f := configured(t)
	f.repo.rateCompleted = repeatedOffTopic(testNow.Add(-time.Hour))[:4]
	f.consent.hook = func() { f.repo.rateCompleted = repeatedOffTopic(testNow.Add(-time.Hour)) }
	_, err := f.s.Ask(t.Context(), user, request(), "question", "with")
	var limited *RateLimitError
	if !errors.As(err, &limited) || f.repo.rateCalls != 2 || f.repo.used != 0 || f.keys.releases != 1 || len(f.starter.started) != 0 {
		t.Fatalf("concurrent fifth outcome admitted: %v %v", err, *f.events)
	}
	want := []string{"begin", "guard", "reserve", "off-topic", "commit", "consent", "ready", "completion", "begin", "guard", "finalize", "lock", "off-topic", "rollback", "begin", "release", "commit"}
	if !reflect.DeepEqual(*f.events, want) {
		t.Fatalf("%v", *f.events)
	}
}

func repeatedOffTopic(at time.Time) []time.Time { return []time.Time{at, at, at, at, at} }

// grpc-api.md 5.2: a missing or foreign answer (404) is reported before consent
// and the daily limit (403); a blocked user still gets 429 first.
func TestUnitRetryNotFoundPrecedesConsentAndDailyLimit(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*setup)
		want   error
		events []string
	}{
		{"missing", func(*setup) {}, ErrNotFound, []string{"begin", "guard", "reserve", "off-topic", "exists", "rollback", "begin", "release", "commit"}},
		{"missing without consent", func(f *setup) { f.consent.grant = false }, ErrNotFound, nil},
		{"missing with consent unavailable", func(f *setup) { f.consent.err = errDependency }, ErrNotFound, nil},
		{"missing with daily limit spent", func(f *setup) { f.repo.state = account.State{LimitDay: testNow, Used: 5} }, ErrNotFound, nil},
		{"blocked and missing", func(f *setup) { f.repo.rateCompleted = repeatedOffTopic(testNow.Add(-time.Hour)) }, nil, []string{"begin", "guard", "reserve", "off-topic", "rollback", "begin", "release", "commit"}},
		{"lookup failure", func(f *setup) { f.repo.existsErr = errDependency }, errDependency, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := configured(t)
			if tc.name != "lookup failure" {
				f.repo.existsErr = ErrNotFound
			}
			tc.change(&f)
			_, err := f.s.Retry(t.Context(), user, request(), "foreign")
			if tc.want == nil {
				var limited *RateLimitError
				if !errors.As(err, &limited) || f.repo.existsCalls != 0 {
					t.Fatalf("429 did not precede 404: %v %v", err, *f.events)
				}
			} else if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v: %v", err, tc.want, *f.events)
			}
			joined := strings.Join(*f.events, ",")
			if strings.Contains(joined, "consent") || strings.Contains(joined, "charge") || f.keys.releases != 1 || len(f.starter.started) != 0 {
				t.Fatalf("refusal reached consent or charge: %v", *f.events)
			}
			if tc.events != nil && !reflect.DeepEqual(*f.events, tc.events) {
				t.Fatalf("%v", *f.events)
			}
		})
	}
}

// Ask has no object to look up; Retry of an owned answer keeps the full order.
func TestUnitRetryOwnedAnswerChecksExistenceBeforeConsent(t *testing.T) {
	f := configured(t)
	if _, err := f.s.Retry(t.Context(), user, request(), "old answer"); err != nil {
		t.Fatal(err)
	}
	want := []string{"begin", "guard", "reserve", "off-topic", "exists", "commit", "consent", "ready", "completion", "begin", "guard", "finalize", "lock", "off-topic", "charge", "retry", "commit", "start"}
	if !reflect.DeepEqual(*f.events, want) {
		t.Fatalf("%v", *f.events)
	}
	f = configured(t)
	if _, err := f.s.Ask(t.Context(), user, request(), "q", "with"); err != nil || f.repo.existsCalls != 0 {
		t.Fatalf("ask looked up an answer: %v", err)
	}
}
