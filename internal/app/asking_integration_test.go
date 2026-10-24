package app_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"testing"
	"time"

	assistantv1 "github.com/fin-assist/contracts/gen/go/finassist/assistant/v1"
	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	idem "github.com/fin-assist/go-idempotency"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/fin-assist/assistant/internal/adapter/client"
	keyadapter "github.com/fin-assist/assistant/internal/adapter/idempotency"
	"github.com/fin-assist/assistant/internal/adapter/postgres"
	"github.com/fin-assist/assistant/internal/app/apitest"
	"github.com/fin-assist/assistant/internal/asking"
	"github.com/fin-assist/assistant/internal/domain"
)

// This integration invokes the usecase directly. SQL, tombstones, reservations,
// and the consent client's transport are real; no assistant RPC is invoked.
type acceptanceStarter struct {
	t     *testing.T
	env   *apitest.Env
	ready bool
	mu    sync.Mutex
	tasks []domain.GenerationTask
}

func (s *acceptanceStarter) Ready() bool { return s.ready }
func (s *acceptanceStarter) Start(task domain.GenerationTask) {
	s.t.Helper()
	var text string
	if err := s.env.App.DB.Reader(context.Background()).QueryRow(context.Background(), `SELECT text FROM question WHERE user_id=$1 AND id=$2`, task.UserID, task.QuestionID).Scan(&text); err != nil || text != task.Text {
		s.t.Errorf("runner observed uncommitted question: text=%q err=%v", text, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tasks = append(s.tasks, task)
}
func (s *acceptanceStarter) count() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.tasks) }

type acceptanceIntegration struct {
	env     *apitest.Env
	peers   *generationAPIPeers
	starter *acceptanceStarter
	service *asking.Service
	now     *time.Time
	keys    *idem.Engine
}

func newAcceptanceIntegration(t *testing.T, max int) *acceptanceIntegration {
	t.Helper()
	env := apitest.Start(t)
	peers := &generationAPIPeers{grant: true}
	conn, err := client.Dial("passthrough:///user", client.Options{Peer: "user", Tokens: client.DevToken("assistant"), DialOptions: generationAPIPeerOptions(t, peers)["user"]})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	now := generationAPINow
	keys, err := idem.New(env.App.DB, idem.Options{Clock: func() time.Time { return now }, Registerer: prometheus.NewRegistry(), Methods: map[string]idem.Policy{"ask": {}, "retry": {}, assistantv1.AssistantService_AskQuestion_FullMethodName: {}, assistantv1.AssistantService_RetryAnswer_FullMethodName: {}}})
	if err != nil {
		t.Fatal(err)
	}
	starter := &acceptanceStarter{t: t, env: env, ready: true}
	service := asking.New(env.App.DB, postgres.NewQuestions(env.App.DB), env.App.Tombstones, keyadapter.New(keys), client.NewAssistantConsent(userv1.NewUserInternalServiceClient(conn)), starter, func() time.Time { return now }, max, domain.OffTopicPolicy{Max: 2, Window: time.Hour})
	return &acceptanceIntegration{env: env, peers: peers, starter: starter, service: service, now: &now, keys: keys}
}
func acceptanceKey(endpoint, text string) idem.Request {
	return idem.Request{Scope: generationAPIUser, Endpoint: endpoint, Key: uuid.NewString(), Hash: sha256.Sum256([]byte(text))}
}
func (e *acceptanceIntegration) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := e.env.App.DB.Writer(t.Context()).Exec(t.Context(), sql, args...); err != nil {
		t.Fatal(err)
	}
}
func (e *acceptanceIntegration) assertCounts(t *testing.T, used, questions, answers int) {
	t.Helper()
	var gotUsed, gotQuestions, gotAnswers int
	err := e.env.App.DB.Reader(t.Context()).QueryRow(t.Context(), `SELECT COALESCE((SELECT limit_used FROM user_state WHERE user_id=$1),0),(SELECT count(*) FROM question WHERE user_id=$1),(SELECT count(*) FROM answer WHERE user_id=$1)`, generationAPIUser).Scan(&gotUsed, &gotQuestions, &gotAnswers)
	if err != nil || gotUsed != used || gotQuestions != questions || gotAnswers != answers {
		t.Fatalf("durable counts used/question/answer=%d/%d/%d want%d/%d/%d err=%v", gotUsed, gotQuestions, gotAnswers, used, questions, answers, err)
	}
}
func TestIntegrationAcceptanceReplaysCommittedResultAfterConsentRevocation(t *testing.T) {
	e := newAcceptanceIntegration(t, 3)
	key := acceptanceKey("ask", "saved financial question")
	first, err := e.service.Ask(t.Context(), generationAPIUser, key, "saved financial question", "without")
	if err != nil || first.Remaining != 2 || first.Task.AnswerID == "" {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	e.peers.consent(false, false)
	replay, err := e.service.Ask(t.Context(), generationAPIUser, key, "saved financial question", "without")
	if err != nil || replay != first || e.starter.count() != 1 || e.peers.count("consent") != 1 {
		t.Fatalf("replay=%+v err=%v starts=%d consent=%d", replay, err, e.starter.count(), e.peers.count("consent"))
	}
	e.assertCounts(t, 1, 1, 1)
}
func TestIntegrationAcceptanceReleasesRefusedReservations(t *testing.T) {
	for _, scenario := range []string{"consent", "dependency", "runner"} {
		t.Run(scenario, func(t *testing.T) {
			e := newAcceptanceIntegration(t, 3)
			key := acceptanceKey("ask", scenario)
			want := asking.ErrUnavailable
			switch scenario {
			case "consent":
				e.peers.consent(false, false)
				want = asking.ErrConsent
			case "dependency":
				e.peers.consent(true, true)
			case "runner":
				e.starter.ready = false
			}
			if got, err := e.service.Ask(t.Context(), generationAPIUser, key, scenario, "without"); !errors.Is(err, want) || got.Task.AnswerID != "" {
				t.Fatalf("refusal=%+v %v want%v", got, err, want)
			}
			e.assertCounts(t, 0, 0, 0)
			e.peers.consent(true, false)
			e.starter.ready = true
			if _, err := e.service.Ask(t.Context(), generationAPIUser, key, scenario, "without"); err != nil {
				t.Fatalf("same key after recovery: %v", err)
			}
			e.assertCounts(t, 1, 1, 1)
		})
	}
}
func TestIntegrationRetryRollsBackChargeAndCanReuseRefusedKey(t *testing.T) {
	e := newAcceptanceIntegration(t, 4)
	first, err := e.service.Ask(t.Context(), generationAPIUser, acceptanceKey("ask", "original"), "original", "without")
	if err != nil {
		t.Fatal(err)
	}
	retryKey := acceptanceKey("retry", first.Task.AnswerID)
	if _, err := e.service.Retry(t.Context(), generationAPIUser, retryKey, first.Task.AnswerID); !errors.Is(err, asking.ErrRetryDenied) {
		t.Fatalf("generating retry=%v", err)
	}
	e.assertCounts(t, 1, 1, 1)
	e.exec(t, `UPDATE answer SET status='failed',error_code='MODEL_ERROR',retryable=true,charged=false WHERE user_id=$1 AND id=$2`, generationAPIUser, first.Task.AnswerID)
	e.exec(t, `UPDATE user_state SET limit_used=0 WHERE user_id=$1`, generationAPIUser)
	retried, err := e.service.Retry(t.Context(), generationAPIUser, retryKey, first.Task.AnswerID)
	if err != nil || retried.Task.QuestionID != first.Task.QuestionID || retried.Task.AnswerID == first.Task.AnswerID || retried.Task.Text != "original" || retried.Remaining != 3 {
		t.Fatalf("retried=%+v err=%v", retried, err)
	}
	e.assertCounts(t, 1, 1, 2)
	var replaced bool
	if err := e.env.App.DB.Reader(t.Context()).QueryRow(t.Context(), `SELECT replaced_at IS NOT NULL FROM answer WHERE user_id=$1 AND id=$2`, generationAPIUser, first.Task.AnswerID).Scan(&replaced); err != nil || !replaced {
		t.Fatalf("old attempt not replaced: %v %v", replaced, err)
	}
	replay, err := e.service.Retry(t.Context(), generationAPIUser, retryKey, first.Task.AnswerID)
	if err != nil || replay != retried || e.starter.count() != 2 {
		t.Fatalf("retry replay=%+v err=%v starts=%d", replay, err, e.starter.count())
	}
	e.assertCounts(t, 1, 1, 2)
}
func TestIntegrationConcurrentAcceptanceSerializesDailyLimit(t *testing.T) {
	e := newAcceptanceIntegration(t, 1)
	results := make(chan error, 2)
	start := make(chan struct{})
	for _, text := range []string{"first", "second"} {
		go func() {
			<-start
			_, err := e.service.Ask(t.Context(), generationAPIUser, acceptanceKey("ask", text), text, "without")
			results <- err
		}()
	}
	close(start)
	accepted, limited := 0, 0
	for range 2 {
		err := <-results
		var limit *asking.LimitError
		switch {
		case err == nil:
			accepted++
		case errors.As(err, &limit):
			limited++
		default:
			t.Fatalf("unexpected competing acceptance=%v", err)
		}
	}
	if accepted != 1 || limited != 1 || e.starter.count() != 1 {
		t.Fatalf("accepted=%d limited=%d starts=%d", accepted, limited, e.starter.count())
	}
	e.assertCounts(t, 1, 1, 1)
}
func TestIntegrationAcceptanceOffTopicExpiryDoesNotChargeRefusal(t *testing.T) {
	e := newAcceptanceIntegration(t, 5)
	for range 2 {
		reply, err := e.service.Ask(t.Context(), generationAPIUser, acceptanceKey("ask", uuid.NewString()), "question", "without")
		if err != nil {
			t.Fatal(err)
		}
		e.exec(t, `UPDATE answer SET status='failed',error_code='OFF_TOPIC',retryable=false,charged=false WHERE user_id=$1 AND id=$2`, generationAPIUser, reply.Task.AnswerID)
		e.exec(t, `INSERT INTO off_topic_event(user_id,answer_id,completed_at) VALUES($1,$2,$3)`, generationAPIUser, reply.Task.AnswerID, *e.now)
		e.exec(t, `UPDATE user_state SET limit_used=0 WHERE user_id=$1`, generationAPIUser)
	}
	key := acceptanceKey("ask", "after cooling")
	var rate *asking.RateLimitError
	if _, err := e.service.Ask(t.Context(), generationAPIUser, key, "after cooling", "without"); !errors.As(err, &rate) || !rate.RetryAt.Equal(e.now.Add(time.Hour)) {
		t.Fatalf("rate=%+v err=%v", rate, err)
	}
	e.assertCounts(t, 0, 2, 2)
	*e.now = e.now.Add(time.Hour)
	if _, err := e.service.Ask(t.Context(), generationAPIUser, key, "after cooling", "without"); err != nil {
		t.Fatalf("exact expiry still refused: %v", err)
	}
	e.assertCounts(t, 1, 3, 3)
}
