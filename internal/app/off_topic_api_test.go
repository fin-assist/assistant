package app_test

import (
	"fmt"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	assistantv1 "github.com/fin-assist/contracts/gen/go/finassist/assistant/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	deletionv1 "github.com/fin-assist/contracts/gen/go/finassist/userdeletion/v1"
	settingsv1 "github.com/fin-assist/contracts/gen/go/finassist/usersettings/v1"
	"github.com/fin-assist/contracts/gen/go/finerr"

	"github.com/fin-assist/assistant/internal/app/apitest"
	"github.com/fin-assist/assistant/internal/config"
)

const offTopicAPIText = "Помощник отвечает на финансовые вопросы. Вопрос не потрачен"

func offTopicAPIEnv(t *testing.T, p *generationAPIPeers, m *generationAPIHTTP, mutate ...func(*config.Config)) *apitest.Env {
	t.Helper()
	return generationAPIEnv(t, p, m, append([]func(*config.Config){func(c *config.Config) { c.OffTopic.Text = offTopicAPIText }}, mutate...)...)
}

// These are provider SSE fixtures, not a second generation implementation.
// All function calls have complete lifecycles and share one completed response.
func offTopicAPIBatch(siblings bool) string {
	items := []map[string]any{{"id": "off", "type": "function_call", "call_id": "off", "name": "off_topic", "arguments": "{}", "status": "completed"}}
	if siblings {
		items = append(items,
			map[string]any{"id": "read", "type": "function_call", "call_id": "read", "name": "get_data_overview", "arguments": "{}", "status": "completed"},
			map[string]any{"id": "search", "type": "web_search_call", "status": "completed"},
			map[string]any{"id": "message", "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "Неподтверждённый посторонний ответ", "annotations": []any{map[string]any{"type": "url_citation", "url": "javascript:alert(1)", "title": "Недоверенная ссылка"}}}}})
		// Put the terminal decision last: an implementation executing siblings
		// eagerly would leak a financial read before noticing off_topic.
		items = append(items[1:], items[0])
	}
	events := []map[string]any{generationAPICreated()}
	for index, item := range items {
		id := item["id"]
		added := map[string]any{"id": id, "type": item["type"], "status": "in_progress"}
		switch item["type"] {
		case "function_call":
			added["call_id"], added["name"], added["arguments"] = item["call_id"], item["name"], ""
		case "message":
			added["role"], added["content"] = "assistant", []any{}
		}
		events = append(events, map[string]any{"type": "response.output_item.added", "output_index": index, "item": added})
		switch item["type"] {
		case "function_call":
			events = append(events,
				map[string]any{"type": "response.function_call_arguments.delta", "output_index": index, "item_id": id, "delta": item["arguments"]},
				map[string]any{"type": "response.function_call_arguments.done", "output_index": index, "item_id": id, "name": item["name"], "arguments": item["arguments"]})
		case "message":
			part := item["content"].([]any)[0].(map[string]any)
			events = append(events,
				map[string]any{"type": "response.content_part.added", "output_index": index, "item_id": id, "content_index": 0, "part": map[string]any{"type": "output_text", "text": ""}},
				map[string]any{"type": "response.output_text.delta", "output_index": index, "item_id": id, "content_index": 0, "delta": part["text"]},
				map[string]any{"type": "response.output_text.done", "output_index": index, "item_id": id, "content_index": 0, "text": part["text"]},
				map[string]any{"type": "response.content_part.done", "output_index": index, "item_id": id, "content_index": 0, "part": part})
		}
		events = append(events, map[string]any{"type": "response.output_item.done", "output_index": index, "item": item})
	}
	events = append(events, map[string]any{"type": "response.completed", "response": map[string]any{"id": "r", "status": "completed", "output": items, "usage": map[string]any{"input_tokens": 10, "output_tokens": 4, "total_tokens": 14}}})
	return generationAPIFrames(events...)
}

func offTopicAPICount(t *testing.T, env *apitest.Env, user string) int {
	t.Helper()
	var count int
	if err := env.App.DB.Writer(t.Context()).QueryRow(t.Context(), `SELECT count(*) FROM off_topic_event WHERE user_id=$1`, user).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func offTopicAPIAssertFailure(t *testing.T, env *apitest.Env, user, answer string) {
	t.Helper()
	done, failure := generationAPITerminal(t, env, user, answer)
	if done != nil || failure.GetCode() != assistantv1.AnswerErrorCode_ANSWER_ERROR_CODE_OFF_TOPIC || failure.GetRetryable() || failure.GetText() != offTopicAPIText {
		t.Fatalf("not a sanitized nonretryable OFF_TOPIC: done=%v error=%v", done, failure)
	}
	// Reconnect returns the durable terminal state, with no previous fragments.
	snapshot := presenterSnapshot(t, env, user, answer)
	if snapshot.GetStatus() != assistantv1.AnswerStatus_ANSWER_STATUS_FAILED || len(snapshot.Blocks) != 0 || len(snapshot.Chips) != 0 || snapshot.Source != nil {
		t.Fatalf("failed reconnect retained content: %v", snapshot)
	}
	c := assistantv1.NewAssistantServiceClient(env.Conn)
	page, err := c.ListMessages(generationAPIContext(t, user, ""), &assistantv1.ListMessagesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var saved *assistantv1.Message
	for _, message := range page.Messages {
		if message.Id == answer {
			saved = message
		}
	}
	if saved == nil || saved.GetErrorCode() != assistantv1.AnswerErrorCode_ANSWER_ERROR_CODE_OFF_TOPIC || saved.GetErrorText() != offTopicAPIText || saved.GetRetryable() || len(saved.Blocks) != 0 || len(saved.Chips) != 0 || saved.Source != nil {
		t.Fatalf("failed history retained content/policy: %v", saved)
	}
	var charged, cleared, retryable bool
	var blocks int
	err = env.App.DB.Writer(t.Context()).QueryRow(t.Context(), `SELECT charged,retryable,
 chips='[]'::jsonb AND calculated_at IS NULL AND data_from IS NULL AND data_to IS NULL,
 (SELECT count(*) FROM answer_block b WHERE b.user_id=a.user_id AND b.answer_id=a.id)
 FROM answer a WHERE a.user_id=$1 AND a.id=$2`, user, answer).Scan(&charged, &retryable, &cleared, &blocks)
	if err != nil || charged || retryable || !cleared || blocks != 0 {
		t.Fatalf("terminal did not refund/clear saved data: charged%v retryable%v cleared%v blocks%d %v", charged, retryable, cleared, blocks, err)
	}
}

func offTopicAPIAssertRateLimit(t *testing.T, err error, retryAt time.Time) {
	t.Helper()
	generationAPIAssertError(t, err, codes.ResourceExhausted, "RATE_LIMITED")
	rest, ok := finerr.ToREST(err)
	gotAt, parseErr := time.Parse(time.RFC3339, rest.Metadata["retry_at"])
	expectedAt := retryAt.Truncate(time.Second)
	if retryAt.Nanosecond() != 0 {
		expectedAt = expectedAt.Add(time.Second)
	}
	if !ok || rest.Spec.HTTPStatus != 429 || parseErr != nil || !gotAt.Equal(expectedAt) || rest.RetryAfter <= 0 {
		t.Fatalf("wrong rolling retry time: %+v want%s", rest, retryAt)
	}
	found := false
	for _, detail := range status.Convert(err).Details() {
		if retry, ok := detail.(*errdetails.RetryInfo); ok {
			found = retry.RetryDelay.AsDuration() > 0
		}
	}
	if !found {
		t.Fatal("RATE_LIMITED omitted RetryInfo")
	}
}

func offTopicAPIFive(t *testing.T, env *apitest.Env) []*assistantv1.AskQuestionResponse {
	t.Helper()
	var answers []*assistantv1.AskQuestionResponse
	for i := range 5 {
		accepted := generationAPIAsk(t, env, generationAPIUser, generationAPIKey(), fmt.Sprintf("Посторонний вопрос %d", i), commonv1.TransferMode_TRANSFER_MODE_WITH)
		offTopicAPIAssertFailure(t, env, generationAPIUser, accepted.AnswerId)
		answers = append(answers, accepted)
	}
	return answers
}

func TestAPIOffTopicAPIValidDecisionWinsOverReadSearchAndText(t *testing.T) {
	for _, siblings := range []bool{false, true} {
		t.Run(fmt.Sprint(siblings), func(t *testing.T) {
			p := &generationAPIPeers{grant: true}
			m := generationAPIModel(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, offTopicAPIBatch(siblings))
			})
			env := offTopicAPIEnv(t, p, m)
			accepted := generationAPIAsk(t, env, generationAPIUser, generationAPIKey(), "Не финансовый вопрос", commonv1.TransferMode_TRANSFER_MODE_WITH)
			offTopicAPIAssertFailure(t, env, generationAPIUser, accepted.AnswerId)
			if m.count.Load() != 1 || p.count("overview") != 0 || p.count("aggregate") != 0 || p.count("analytics") != 0 || offTopicAPICount(t, env, generationAPIUser) != 1 {
				t.Fatal("off_topic allowed a sibling RPC or another model step")
			}
			limit, err := assistantv1.NewAssistantServiceClient(env.Conn).GetLimit(generationAPIContext(t, generationAPIUser, ""), &assistantv1.GetLimitRequest{})
			if err != nil || limit.Remaining != 5 {
				t.Fatalf("off_topic consumed daily attempt: %v %v", limit, err)
			}
			if siblings {
				families, err := env.App.Registry.(prometheus.Gatherer).Gather()
				if err != nil {
					t.Fatal(err)
				}
				var searches float64
				for _, family := range families {
					if family.GetName() == "finassist_assistant_search_calls_total" {
						searches = family.Metric[0].GetCounter().GetValue()
					}
				}
				if searches != 1 {
					t.Fatalf("already executed provider search not accounted: %v", searches)
				}
			}
		})
	}
}

func TestAPIOffTopicAPIClearsEarlierPresentationAndSource(t *testing.T) {
	p := &generationAPIPeers{grant: true, aggregateResponse: presenterAggregate()}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	m := generationAPIModel(t, func(n int, w http.ResponseWriter, r *http.Request) {
		switch n {
		case 1:
			_, _ = io.WriteString(w, generationAPIFunctionID("aggregate", "aggregate_operations", `{"all_time":true,"group_by":["category"]}`))
		case 2:
			_, _ = io.WriteString(w, generationAPIFunctionID("present", "present_result", `{"result_id":"aggregate","view":"rows"}`))
		case 3:
			close(entered)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			_, _ = io.WriteString(w, offTopicAPIBatch(true))
		default:
			t.Error("off_topic continued model execution")
		}
	})
	env := offTopicAPIEnv(t, p, m)
	accepted := generationAPIAsk(t, env, generationAPIUser, generationAPIKey(), "Вопрос с поздним OFF_TOPIC", commonv1.TransferMode_TRANSFER_MODE_WITHOUT)
	presenterWait(t, entered)
	before := presenterSnapshot(t, env, generationAPIUser, accepted.AnswerId)
	if len(before.Blocks) == 0 || before.Blocks[0].GetRows() == nil || p.count("aggregate") != 1 {
		t.Fatalf("test did not establish presentation from a required source-bearing read: %v", before)
	}
	unblock()
	offTopicAPIAssertFailure(t, env, generationAPIUser, accepted.AnswerId)
	if p.count("aggregate") != 1 || p.count("overview") != 0 || m.count.Load() != 3 {
		t.Fatal("late off_topic allowed sibling or subsequent calls")
	}
}

func TestAPIOffTopicAPIFiveRealTerminalsBlockNewCallsButReplayAndReadsSurvive(t *testing.T) {
	p := &generationAPIPeers{grant: true}
	m := generationAPIModel(t, func(_ int, w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, offTopicAPIBatch(false)) })
	env := offTopicAPIEnv(t, p, m)
	c := assistantv1.NewAssistantServiceClient(env.Conn)
	canRetry := true
	_, old := generationAPISeedAnswer(t, env, "failed", "MODEL_ERROR", &canRetry)
	retryKey := generationAPIKey()
	retryRequest := &assistantv1.RetryAnswerRequest{AnswerId: old}
	retried, err := c.RetryAnswer(generationAPIContext(t, generationAPIUser, retryKey), retryRequest)
	if err != nil {
		t.Fatal(err)
	}
	offTopicAPIAssertFailure(t, env, generationAPIUser, retried.AnswerId)
	askKey := generationAPIKey()
	askRequest := &assistantv1.AskQuestionRequest{Text: "Первый посторонний вопрос"}
	asked, err := c.AskQuestion(generationAPIContext(t, generationAPIUser, askKey), askRequest)
	if err != nil {
		t.Fatal(err)
	}
	offTopicAPIAssertFailure(t, env, generationAPIUser, asked.AnswerId)
	for i := range 3 {
		accepted := generationAPIAsk(t, env, generationAPIUser, generationAPIKey(), fmt.Sprintf("Ещё вопрос %d", i), commonv1.TransferMode_TRANSFER_MODE_WITH)
		offTopicAPIAssertFailure(t, env, generationAPIUser, accepted.AnswerId)
	}
	if offTopicAPICount(t, env, generationAPIUser) != 5 || m.count.Load() != 5 {
		t.Fatal("test did not complete five real model cycles")
	}
	questions, answers, keys := generationAPICount(t, env, "question"), generationAPICount(t, env, "answer"), generationAPICount(t, env, "key")
	blockedKey := generationAPIKey()
	_, err = c.AskQuestion(generationAPIContext(t, generationAPIUser, blockedKey), &assistantv1.AskQuestionRequest{Text: "Блокируемый вопрос"})
	offTopicAPIAssertRateLimit(t, err, generationAPINow.Add(7*24*time.Hour))
	_, err = c.RetryAnswer(generationAPIContext(t, generationAPIUser, generationAPIKey()), &assistantv1.RetryAnswerRequest{AnswerId: old})
	offTopicAPIAssertRateLimit(t, err, generationAPINow.Add(7*24*time.Hour))
	if generationAPICount(t, env, "question") != questions || generationAPICount(t, env, "answer") != answers || generationAPICount(t, env, "key") != keys || m.count.Load() != 5 {
		t.Fatal("rate refusal stored key, charged, created or called model")
	}
	p.consent(false, false)
	consents := p.count("consent")
	askReplay, err := c.AskQuestion(generationAPIContext(t, generationAPIUser, askKey), askRequest)
	if err != nil || !proto.Equal(asked, askReplay) {
		t.Fatalf("Ask replay did not bypass weekly gate/revoked consent: %v %v", askReplay, err)
	}
	retryReplay, err := c.RetryAnswer(generationAPIContext(t, generationAPIUser, retryKey), retryRequest)
	if err != nil || !proto.Equal(retried, retryReplay) || p.count("consent") != consents || m.count.Load() != 5 {
		t.Fatalf("Retry replay called policy/model: %v %v", retryReplay, err)
	}
	limit, err := c.GetLimit(generationAPIContext(t, generationAPIUser, ""), &assistantv1.GetLimitRequest{})
	if err != nil || limit.Remaining != 5 {
		t.Fatalf("daily limit altered by weekly refusal: %v %v", limit, err)
	}
	if _, err := settingsv1.NewUserSettingsSyncServiceClient(env.Conn).SetUserTimezone(apitest.Caller(t.Context(), "user"), &settingsv1.SetUserTimezoneRequest{UserId: generationAPIUser, Timezone: "Pacific/Auckland", SettingsVersion: 1}); err != nil {
		t.Fatal(err)
	}
	p.consent(true, false)
	_, err = c.AskQuestion(generationAPIContext(t, generationAPIUser, blockedKey), &assistantv1.AskQuestionRequest{Text: "Блокируемый вопрос"})
	offTopicAPIAssertRateLimit(t, err, generationAPINow.Add(7*24*time.Hour))
	other := generationAPIAsk(t, env, generationAPIOther, generationAPIKey(), "Другой пользователь", commonv1.TransferMode_TRANSFER_MODE_WITH)
	offTopicAPIAssertFailure(t, env, generationAPIOther, other.AnswerId)
	if offTopicAPICount(t, env, generationAPIUser) != 5 || offTopicAPICount(t, env, generationAPIOther) != 1 {
		t.Fatal("timezone/read/foreign-user activity reset another user's ledger")
	}
	stream, err := c.WatchAnswer(generationAPIContext(t, generationAPIOther, ""), &assistantv1.WatchAnswerRequest{AnswerId: asked.AnswerId})
	if err == nil {
		_, err = stream.Recv()
	}
	generationAPIAssertError(t, err, codes.NotFound, "NOT_FOUND")
	if _, err := deletionv1.NewUserDeletionServiceClient(env.Conn).PurgeUserData(apitest.Caller(t.Context(), "user"), &deletionv1.PurgeUserDataRequest{UserId: generationAPIUser}); err != nil {
		t.Fatal(err)
	}
	if offTopicAPICount(t, env, generationAPIUser) != 0 || offTopicAPICount(t, env, generationAPIOther) != 1 {
		t.Fatal("deletion did not cascade only its own ledger")
	}
}

func TestAPIOffTopicAPIExactSevenDayExpiryReleasesPreviouslyRefusedKey(t *testing.T) {
	p := &generationAPIPeers{grant: true}
	m := generationAPIModel(t, func(_ int, w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, offTopicAPIBatch(false)) })
	env := offTopicAPIEnv(t, p, m)
	answers := offTopicAPIFive(t, env)
	c := assistantv1.NewAssistantServiceClient(env.Conn)
	key := generationAPIKey()
	request := &assistantv1.AskQuestionRequest{Text: "Повтор после точной границы"}
	// Arrange the first real terminal's age; the acceptance clock stays fixed.
	boundary := generationAPINow.Add(-7 * 24 * time.Hour)
	if _, err := env.App.DB.Writer(t.Context()).Exec(t.Context(), `UPDATE off_topic_event SET completed_at=$3 WHERE user_id=$1 AND answer_id=$2`, generationAPIUser, answers[0].AnswerId, boundary.Add(time.Microsecond)); err != nil {
		t.Fatal(err)
	}
	_, err := c.AskQuestion(generationAPIContext(t, generationAPIUser, key), request)
	offTopicAPIAssertRateLimit(t, err, generationAPINow.Add(time.Microsecond))
	if _, err := env.App.DB.Writer(t.Context()).Exec(t.Context(), `UPDATE off_topic_event SET completed_at=$3 WHERE user_id=$1 AND answer_id=$2`, generationAPIUser, answers[0].AnswerId, boundary); err != nil {
		t.Fatal(err)
	}
	accepted, err := c.AskQuestion(generationAPIContext(t, generationAPIUser, key), request)
	if err != nil {
		t.Fatalf("same refused key did not pass at exact expiry: %v", err)
	}
	offTopicAPIAssertFailure(t, env, generationAPIUser, accepted.AnswerId)
	if m.count.Load() != 6 || generationAPICount(t, env, "question") != 6 {
		t.Fatal("expiry retry duplicated or failed to create exactly one question")
	}
}

func TestAPIOffTopicAPIRetryWithSameRefusedKeySucceedsAfterExpiry(t *testing.T) {
	m := generationAPIModel(t, func(_ int, w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, offTopicAPIBatch(false)) })
	env := offTopicAPIEnv(t, &generationAPIPeers{grant: true}, m)
	canRetry := true
	question, old := generationAPISeedAnswer(t, env, "failed", "MODEL_ERROR", &canRetry)
	completed := offTopicAPIFive(t, env)
	c := assistantv1.NewAssistantServiceClient(env.Conn)
	key := generationAPIKey()
	request := &assistantv1.RetryAnswerRequest{AnswerId: old}
	_, err := c.RetryAnswer(generationAPIContext(t, generationAPIUser, key), request)
	offTopicAPIAssertRateLimit(t, err, generationAPINow.Add(7*24*time.Hour))
	if _, err := env.App.DB.Writer(t.Context()).Exec(t.Context(), `UPDATE off_topic_event SET completed_at=$3 WHERE user_id=$1 AND answer_id=$2`, generationAPIUser, completed[0].AnswerId, generationAPINow.Add(-7*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	accepted, err := c.RetryAnswer(generationAPIContext(t, generationAPIUser, key), request)
	if err != nil || accepted.AnswerId == old {
		t.Fatalf("previous 429 pinned retry key: %v %v", accepted, err)
	}
	offTopicAPIAssertFailure(t, env, generationAPIUser, accepted.AnswerId)
	var savedQuestion string
	if err := env.App.DB.Writer(t.Context()).QueryRow(t.Context(), `SELECT question_id::text FROM answer WHERE user_id=$1 AND id=$2`, generationAPIUser, accepted.AnswerId).Scan(&savedQuestion); err != nil || savedQuestion != question || generationAPICount(t, env, "question") != 6 || m.count.Load() != 6 {
		t.Fatalf("expiry retry changed original question or duplicated generation: %s %v", savedQuestion, err)
	}
}

func TestAPIOffTopicAPISevenInflightCompletionsWaitForThirdExpiry(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	m := generationAPIModel(t, func(_ int, w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, offTopicAPIBatch(false))
	})
	env := offTopicAPIEnv(t, &generationAPIPeers{grant: true}, m, func(c *config.Config) { c.DailyMax = 10; c.DB.MaxConns = 10 })
	var accepted []*assistantv1.AskQuestionResponse
	for i := range 7 {
		accepted = append(accepted, generationAPIAsk(t, env, generationAPIUser, generationAPIKey(), fmt.Sprintf("Параллельный вопрос %d", i), commonv1.TransferMode_TRANSFER_MODE_WITH))
		generationAPIWaitArrival(t, m)
	}
	unblock()
	for _, answer := range accepted {
		offTopicAPIAssertFailure(t, env, generationAPIUser, answer.AnswerId)
	}
	if offTopicAPICount(t, env, generationAPIUser) != 7 || m.count.Load() != 7 {
		t.Fatal("already accepted parallel work did not finish exactly once")
	}
	// Give real committed ledger entries distinct ages to distinguish the third
	// expiry from the first; no synthetic generation or outcome is inserted.
	for i, answer := range accepted {
		completed := generationAPINow.Add(time.Duration(i-6) * 24 * time.Hour)
		if _, err := env.App.DB.Writer(t.Context()).Exec(t.Context(), `UPDATE off_topic_event SET completed_at=$3 WHERE user_id=$1 AND answer_id=$2`, generationAPIUser, answer.AnswerId, completed); err != nil {
			t.Fatal(err)
		}
	}
	_, err := assistantv1.NewAssistantServiceClient(env.Conn).AskQuestion(generationAPIContext(t, generationAPIUser, generationAPIKey()), &assistantv1.AskQuestionRequest{Text: "Восьмой вопрос"})
	offTopicAPIAssertRateLimit(t, err, generationAPINow.Add(3*24*time.Hour))
	if m.count.Load() != 7 || generationAPICount(t, env, "question") != 7 {
		t.Fatal("overflow guard let another question run")
	}
}

func TestAPIOffTopicAPIOnlyChargedFinancialCompletionResetsBlockedUser(t *testing.T) {
	for _, outcome := range []string{"charged", "complete-free", "cannot-answer", "model-error"} {
		t.Run(outcome, func(t *testing.T) {
			release := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			m := generationAPIModel(t, func(n int, w http.ResponseWriter, r *http.Request) {
				if n == 1 {
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
					switch outcome {
					case "charged":
						_, _ = io.WriteString(w, generationAPIText("Готовый финансовый ответ"))
					case "complete-free":
						_, _ = io.WriteString(w, generationAPIFunction("get_analytics", `{"period":"month"}`))
					case "cannot-answer":
						_, _ = io.WriteString(w, generationAPIFunction("cannot_answer", `{}`))
					case "model-error":
						w.WriteHeader(http.StatusInternalServerError)
					}
					return
				}
				if outcome == "complete-free" && n == 7 {
					_, _ = io.WriteString(w, generationAPIText("За период нет данных"))
					return
				}
				_, _ = io.WriteString(w, offTopicAPIBatch(false))
			})
			env := offTopicAPIEnv(t, &generationAPIPeers{grant: true}, m)
			c := assistantv1.NewAssistantServiceClient(env.Conn)
			financial := generationAPIAsk(t, env, generationAPIUser, generationAPIKey(), "Финансовый вопрос уже выполняется", commonv1.TransferMode_TRANSFER_MODE_WITH)
			generationAPIWaitArrival(t, m)
			offTopicAPIFive(t, env)
			key := generationAPIKey()
			request := &assistantv1.AskQuestionRequest{Text: "Тот же ключ после успешного финансового ответа"}
			_, err := c.AskQuestion(generationAPIContext(t, generationAPIUser, key), request)
			offTopicAPIAssertRateLimit(t, err, generationAPINow.Add(7*24*time.Hour))
			unblock()
			done, failed := generationAPITerminal(t, env, generationAPIUser, financial.AnswerId)
			if outcome == "charged" || outcome == "complete-free" {
				if failed != nil || done == nil || done.Charged != (outcome == "charged") {
					t.Fatalf("unexpected financial outcome: done%v error%v", done, failed)
				}
			} else {
				want := assistantv1.AnswerErrorCode_ANSWER_ERROR_CODE_MODEL_ERROR
				if outcome == "cannot-answer" {
					want = assistantv1.AnswerErrorCode_ANSWER_ERROR_CODE_CANNOT_ANSWER
				}
				if done != nil || failed.GetCode() != want {
					t.Fatalf("unexpected terminal failure: done%v error%v", done, failed)
				}
			}
			if outcome == "charged" {
				if offTopicAPICount(t, env, generationAPIUser) != 0 {
					t.Fatal("charged financial completion did not reset existing five events")
				}
				accepted, err := c.AskQuestion(generationAPIContext(t, generationAPIUser, key), request)
				if err != nil {
					t.Fatalf("reset did not release previously refused key: %v", err)
				}
				offTopicAPIAssertFailure(t, env, generationAPIUser, accepted.AnswerId)
				var retained int
				if err := env.App.DB.Writer(t.Context()).QueryRow(t.Context(), `SELECT count(*) FROM answer WHERE user_id=$1 AND error_code='OFF_TOPIC'`, generationAPIUser).Scan(&retained); err != nil || retained != 6 || offTopicAPICount(t, env, generationAPIUser) != 1 {
					t.Fatalf("reset deleted history or failed new counting: history%d %v", retained, err)
				}
			} else {
				beforeHTTP := m.count.Load()
				_, err := c.AskQuestion(generationAPIContext(t, generationAPIUser, key), request)
				offTopicAPIAssertRateLimit(t, err, generationAPINow.Add(7*24*time.Hour))
				if offTopicAPICount(t, env, generationAPIUser) != 5 || m.count.Load() != beforeHTTP {
					t.Fatal("free completion or error reset/continued model under blocked policy")
				}
			}
		})
	}
}

// grpc-api.md 7.4 and 5.2: the off-topic block (429) is checked on primary before
// GetAssistantConsentStatus, so a blocked user with revoked consent, or while user
// is unavailable, gets RATE_LIMITED and the user service is not called at all.
func TestAPIOffTopicAPIBlockPrecedesConsentRead(t *testing.T) {
	for _, tc := range []struct {
		name               string
		grant, unavailable bool
	}{
		{"consent revoked", false, false},
		{"user unavailable", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &generationAPIPeers{grant: true}
			m := generationAPIModel(t, func(_ int, w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, offTopicAPIBatch(false)) })
			env := offTopicAPIEnv(t, p, m)
			canRetry := true
			_, old := generationAPISeedAnswer(t, env, "failed", "MODEL_ERROR", &canRetry)
			offTopicAPIFive(t, env)
			c := assistantv1.NewAssistantServiceClient(env.Conn)
			p.consent(tc.grant, tc.unavailable)
			consents, questions, answers, keys := p.count("consent"), generationAPICount(t, env, "question"), generationAPICount(t, env, "answer"), generationAPICount(t, env, "key")
			_, err := c.AskQuestion(generationAPIContext(t, generationAPIUser, generationAPIKey()), &assistantv1.AskQuestionRequest{Text: "Вопрос при блокировке"})
			offTopicAPIAssertRateLimit(t, err, generationAPINow.Add(7*24*time.Hour))
			_, err = c.RetryAnswer(generationAPIContext(t, generationAPIUser, generationAPIKey()), &assistantv1.RetryAnswerRequest{AnswerId: old})
			offTopicAPIAssertRateLimit(t, err, generationAPINow.Add(7*24*time.Hour))
			if p.count("consent") != consents {
				t.Fatal("blocked user reached the consent RPC before the 429 check")
			}
			if generationAPICount(t, env, "question") != questions || generationAPICount(t, env, "answer") != answers || generationAPICount(t, env, "key") != keys || m.count.Load() != 5 {
				t.Fatal("refusal stored a key, created a question/answer or called the model")
			}
		})
	}
}
