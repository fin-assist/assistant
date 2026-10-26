package app_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	assistantv1 "github.com/fin-assist/contracts/gen/go/finassist/assistant/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	statementv1 "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"

	"github.com/fin-assist/assistant/internal/config"
	"github.com/fin-assist/assistant/internal/fakellm"
)

func scenarioModel(t *testing.T) *generationAPIHTTP {
	t.Helper()
	h, e := fakellm.Default()
	if e != nil {
		t.Fatal(e)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &generationAPIHTTP{server: srv}
}
func TestAPIFakeModelRowsSearchAndRefunds(t *testing.T) {
	for _, tc := range []struct {
		question string
		failure  assistantv1.AnswerErrorCode
		noData   bool
	}{
		{question: "rows"}, {question: "search"}, {question: "unknown"}, {question: "model-error", failure: assistantv1.AnswerErrorCode_ANSWER_ERROR_CODE_MODEL_ERROR},
		{question: "refusal", failure: assistantv1.AnswerErrorCode_ANSWER_ERROR_CODE_CANNOT_ANSWER},
		{question: "disconnect", failure: assistantv1.AnswerErrorCode_ANSWER_ERROR_CODE_MODEL_ERROR},
		{question: "timeout", failure: assistantv1.AnswerErrorCode_ANSWER_ERROR_CODE_MODEL_ERROR},
		{question: "rows no data", noData: true},
	} {
		t.Run(tc.question, func(t *testing.T) {
			p := &generationAPIPeers{grant: true}
			if strings.HasPrefix(tc.question, "rows") {
				p.aggregateResponse = presenterAggregate()
			}
			if tc.noData {
				p.aggregateResponse.Bounds = &commonv1.DataBounds{Coverage: commonv1.Coverage_COVERAGE_NO_DATA}
				p.aggregateResponse.TotalAmountMinor = 0
				p.aggregateResponse.TotalOperationCount = 0
				p.aggregateResponse.Groups = nil
			}
			m := scenarioModel(t)
			env := generationAPIEnv(t, p, m)
			accepted := generationAPIAsk(t, env, generationAPIUser, generationAPIKey(), tc.question, commonv1.TransferMode_TRANSFER_MODE_WITHOUT)
			done, failure := generationAPITerminal(t, env, generationAPIUser, accepted.AnswerId)
			if tc.failure != 0 {
				if done != nil || failure.GetCode() != tc.failure || failure.GetRemainingLimit() != 5 {
					t.Fatalf("refund: %v %v", done, failure)
				}
				if len(presenterHistory(t, env, generationAPIUser).GetBlocks()) != 0 {
					t.Fatal("partial answer retained")
				}
				return
			}
			if failure != nil || done == nil {
				t.Fatalf("terminal: %v %v", done, failure)
			}
			if tc.noData && done.GetRemainingLimit() != 5 {
				t.Fatal("no-data attempt charged", done)
			}
			saved := presenterHistory(t, env, generationAPIUser)
			if strings.HasPrefix(tc.question, "rows") {
				if p.count("overview") != 1 || p.count("aggregate") != 1 || len(saved.Blocks) < 2 || (!tc.noData && saved.Blocks[0].GetRows() == nil) {
					t.Fatal("real tools or server rows missing", saved)
				}
			}
			if tc.question == "search" {
				found := false
				for _, b := range saved.Blocks {
					if strings.Contains(b.GetText().GetText(), "https://cbr.ru/") {
						found = true
					}
				}
				if !found {
					t.Fatal("provider citation not rendered", saved)
				}
			}
		})
	}
}

func TestAPIFakeModelLimitAndReplay(t *testing.T) {
	p := &generationAPIPeers{grant: true}
	m := scenarioModel(t)
	env := generationAPIEnv(t, p, m, func(c *config.Config) { c.DailyMax = 1 })
	key := generationAPIKey()
	a := generationAPIAsk(t, env, generationAPIUser, key, "unknown", commonv1.TransferMode_TRANSFER_MODE_WITHOUT)
	generationAPITerminal(t, env, generationAPIUser, a.AnswerId)
	replay := generationAPIAsk(t, env, generationAPIUser, key, "unknown", commonv1.TransferMode_TRANSFER_MODE_WITHOUT)
	if !proto.Equal(a, replay) {
		t.Fatal("replay changed", a, replay)
	}
	_, err := assistantv1.NewAssistantServiceClient(env.Conn).AskQuestion(generationAPIContext(t, generationAPIUser, generationAPIKey()), &assistantv1.AskQuestionRequest{Text: "unknown"})
	generationAPIAssertError(t, err, codes.ResourceExhausted, "LIMIT_EXCEEDED")
	if generationAPICount(t, env, "question") != 1 {
		t.Fatal("limit wrote another question")
	}
}

func TestAPIFakeModelHeadersEveryStep(t *testing.T) {
	h, e := fakellm.Default()
	if e != nil {
		t.Fatal(e)
	}
	var mu sync.Mutex
	var headers []http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		headers = append(headers, r.Header.Clone())
		mu.Unlock()
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	p := &generationAPIPeers{grant: true, aggregateResponse: presenterAggregate()}
	env := generationAPIEnv(t, p, &generationAPIHTTP{server: srv})
	a := generationAPIAsk(t, env, generationAPIUser, generationAPIKey(), "rows", commonv1.TransferMode_TRANSFER_MODE_WITHOUT)
	done, failure := generationAPITerminal(t, env, generationAPIUser, a.AnswerId)
	if done == nil || failure != nil {
		t.Fatal(done, failure)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(headers) != 3 {
		t.Fatal(len(headers))
	}
	for _, h := range headers {
		if h.Get("x-data-logging-enabled") != "false" {
			t.Fatal("privacy header missing")
		}
	}
}

func TestAPIFakeModelConcurrentReservationAndCrossPodWatch(t *testing.T) {
	h, e := fakellm.Default()
	if e != nil {
		t.Fatal(e)
	}
	arrived, release := make(chan struct{}, 1), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived <- struct{}{}
		select {
		case <-release:
			h.ServeHTTP(w, r)
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	consentEntered, consentRelease := make(chan struct{}), make(chan struct{})
	var once sync.Once
	p := &generationAPIPeers{grant: true, consentHook: func(ctx context.Context) error {
		once.Do(func() { close(consentEntered) })
		select {
		case <-consentRelease:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	env := generationAPIEnv(t, p, &generationAPIHTTP{server: srv})
	client := assistantv1.NewAssistantServiceClient(env.Conn)
	key := generationAPIKey()
	type acceptedResult struct {
		response *assistantv1.AskQuestionResponse
		err      error
	}
	accepted := make(chan acceptedResult, 1)
	go func() {
		a, e := client.AskQuestion(generationAPIContext(t, generationAPIUser, key), &assistantv1.AskQuestionRequest{Text: "unknown"})
		accepted <- acceptedResult{a, e}
	}()
	presenterWait(t, consentEntered)
	_, err := client.AskQuestion(generationAPIContext(t, generationAPIUser, key), &assistantv1.AskQuestionRequest{Text: "unknown"})
	generationAPIAssertError(t, err, codes.Aborted, "REQUEST_IN_PROGRESS")
	close(consentRelease)
	got := <-accepted
	if got.err != nil {
		t.Fatal(got.err)
	}
	presenterWait(t, arrived)
	other := dialogueReplica(t, env)
	observer := assistantv1.NewAssistantServiceClient(other)
	stream, e := observer.WatchAnswer(generationAPIContext(t, generationAPIUser, ""), &assistantv1.WatchAnswerRequest{AnswerId: got.response.AnswerId})
	if e != nil {
		t.Fatal(e)
	}
	first, e := stream.Recv()
	if e != nil || first.GetSnapshot() == nil {
		t.Fatal(first, e)
	}
	close(release)
	for {
		event, e := stream.Recv()
		if e != nil {
			t.Fatal(e)
		}
		if event.GetError() != nil {
			t.Fatal(event)
		}
		if event.GetDone() != nil {
			break
		}
	}
	replay := generationAPIAsk(t, env, generationAPIUser, key, "unknown", commonv1.TransferMode_TRANSFER_MODE_UNSPECIFIED)
	if !proto.Equal(got.response, replay) {
		t.Fatal("concurrent replay changed answer")
	}
}

func TestAPIFakeModelSelectedLockedMetricRefunds(t *testing.T) {
	p := &generationAPIPeers{grant: true, analyticsResponse: presenterAnalytics(t)}
	p.analyticsResponse.Blocks[0].Status = "locked"
	p.analyticsResponse.Blocks[0].Lock = &commonv1.BlockLock{Reason: "need_full_months", Text: "Нужно 3 полных месяца"}
	env := generationAPIEnv(t, p, scenarioModel(t))
	a := generationAPIAsk(t, env, generationAPIUser, generationAPIKey(), "analytics", commonv1.TransferMode_TRANSFER_MODE_WITHOUT)
	done, failure := generationAPITerminal(t, env, generationAPIUser, a.AnswerId)
	if done == nil || failure != nil || done.GetCharged() || done.GetRemainingLimit() != 5 || p.count("analytics") != 1 {
		t.Fatal(done, failure)
	}
}

func TestAPIFakeModelRetryAnswerAfterModelError(t *testing.T) {
	env := generationAPIEnv(t, &generationAPIPeers{grant: true}, scenarioModel(t))
	original := generationAPIAsk(t, env, generationAPIUser, generationAPIKey(), "model-error", commonv1.TransferMode_TRANSFER_MODE_WITHOUT)
	_, failed := generationAPITerminal(t, env, generationAPIUser, original.AnswerId)
	if failed.GetCode() != assistantv1.AnswerErrorCode_ANSWER_ERROR_CODE_MODEL_ERROR {
		t.Fatal(failed)
	}
	client := assistantv1.NewAssistantServiceClient(env.Conn)
	retry, e := client.RetryAnswer(generationAPIContext(t, generationAPIUser, generationAPIKey()), &assistantv1.RetryAnswerRequest{AnswerId: original.AnswerId})
	if e != nil || retry.GetAnswerId() == original.AnswerId || retry.GetAnswerId() == "" {
		t.Fatal(retry, e)
	}
	_, failed = generationAPITerminal(t, env, generationAPIUser, retry.AnswerId)
	if failed.GetRemainingLimit() != 5 || generationAPICount(t, env, "question") != 1 {
		t.Fatal("retry duplicated the question or kept the charge", failed)
	}
}

func TestAPIFakeModelRepeatedFunctionsKeepDistinctResults(t *testing.T) {
	peers := &generationAPIPeers{grant: true, aggregateHook: func(request *statementv1.AggregateOperationsRequest) *statementv1.AggregateOperationsResponse {
		response := presenterAggregate()
		amount := int64(request.Limit) * 100
		response.TotalAmountMinor = amount
		response.Groups[0].AmountMinor = amount
		response.Groups[0].AverageMinor = amount
		return response
	}}
	env := generationAPIEnv(t, peers, scenarioModel(t))
	accepted := generationAPIAsk(t, env, generationAPIUser, generationAPIKey(), "repeat", commonv1.TransferMode_TRANSFER_MODE_WITHOUT)
	done, failure := generationAPITerminal(t, env, generationAPIUser, accepted.AnswerId)
	if done == nil || failure != nil || !done.GetCharged() || peers.count("aggregate") != 3 {
		t.Fatal(done, failure)
	}
	history := presenterHistory(t, env, generationAPIUser)
	if len(history.Blocks) != 4 {
		t.Fatal("expected three independent rows and final text", history)
	}
	for index, amount := range []int64{100, 200, 300} {
		if history.Blocks[index].GetRows().GetRows()[0].GetAmountMinor() != amount {
			t.Fatal("repeated function overwrote or swapped the result", history)
		}
	}
}
