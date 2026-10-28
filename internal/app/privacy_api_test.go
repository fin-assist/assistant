package app_test

import (
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"

	assistantv1 "github.com/fin-assist/contracts/gen/go/finassist/assistant/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	statementv1 "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestAPIPrivacyFiltersHistoryQuestionToolEchoAndRetry(t *testing.T) {
	const phone = "+7 (999) 123-45-67"
	const oldQuestion = "Старая почта old@example.test"
	const oldAnswer = "Старая карта 4111 1111 1111 1111"
	const question = "Аптека и лекарства: траты по " + phone + ", остаток 100.25 RUB"
	description := "Т-Банк " + phone + "; card: 4111111111111111; account: 40817810099910004312"
	spoken := "unused-spoken-description"
	mask := "··4821"
	p := &generationAPIPeers{grant: true, findResponse: &statementv1.FindOperationsResponse{
		Items:      []*statementv1.Operation{{Id: "10000000-0000-4000-8000-123456789012", Kind: statementv1.OperationKind_OPERATION_KIND_EXPENSE, OccurredAt: timestamppb.New(generationAPINow), AmountMinor: 9007199254740993, Currency: "RUB", Title: "Аптека " + phone, Description: &description, Status: statementv1.OperationStatus_OPERATION_STATUS_POSTED, Account: &commonv1.AccountRef{TypeName: "Текущий", Mask: &mask, Spoken: &spoken}}},
		TotalCount: 1, Range: generationAPIRange(), Bounds: generationAPIBounds(), TimeZone: "Europe/Moscow", Version: &statementv1.DataVersion{},
	}}
	var model *generationAPIHTTP
	var alias string
	model = generationAPIModel(t, func(step int, w http.ResponseWriter, _ *http.Request) {
		model.mu.Lock()
		input := string(model.requests[step-1].body["input"])
		model.mu.Unlock()
		for _, secret := range []string{"old@example.test", "4111 1111", "4111111111111111", "40817810099910004312", phone, "123456789012", "unused-spoken-description"} {
			if strings.Contains(input, secret) {
				t.Errorf("original requisite/identifier reached model: %s", secret)
			}
		}
		if step == 1 {
			_, _ = io.WriteString(w, generationAPIText(oldAnswer))
			return
		}
		if step == 2 || step == 4 {
			tokens := regexp.MustCompile(`\[phone_[0-9a-f]+\]`).FindAllString(input, -1)
			if len(tokens) == 0 {
				t.Error("phone alias missing")
				return
			}
			if step == 4 && tokens[0] == alias {
				t.Error("retry reused prior generation aliases")
			}
			alias = tokens[0]
			args, _ := json.Marshal(map[string]any{"filter": map[string]any{"q": alias}})
			_, _ = io.WriteString(w, generationAPIFunctionID("same-call-name", "find_operations", string(args)))
			return
		}
		if !strings.Contains(input, alias) || !strings.Contains(input, "9007199254740993") || !strings.Contains(input, "100.25 RUB") || !strings.Contains(input, "Аптека") {
			t.Error("values or alias continuity lost")
		}
		if step == 3 {
			w.WriteHeader(503)
			return
		}
		_, _ = io.WriteString(w, generationAPIText("Расходы по найденной операции"))
	})
	env := generationAPIEnv(t, p, model)
	old := generationAPIAsk(t, env, generationAPIUser, generationAPIKey(), oldQuestion, commonv1.TransferMode_TRANSFER_MODE_WITH)
	generationAPITerminal(t, env, generationAPIUser, old.AnswerId)
	accepted := generationAPIAsk(t, env, generationAPIUser, generationAPIKey(), question, commonv1.TransferMode_TRANSFER_MODE_WITH)
	firstDone, firstError := generationAPITerminal(t, env, generationAPIUser, accepted.AnswerId)
	if firstDone != nil || firstError == nil || !firstError.Retryable || firstError.RemainingLimit != 4 {
		t.Fatalf("expected refunded retryable model failure: %v %v", firstDone, firstError)
	}
	retried, err := assistantv1.NewAssistantServiceClient(env.Conn).RetryAnswer(generationAPIContext(t, generationAPIUser, generationAPIKey()), &assistantv1.RetryAnswerRequest{AnswerId: accepted.AnswerId})
	if err != nil {
		t.Fatal(err)
	}
	done, failed := generationAPITerminal(t, env, generationAPIUser, retried.AnswerId)
	if done == nil || failed != nil || model.count.Load() != 5 || p.count("find") != 2 {
		t.Fatalf("retry/filter cycle: done=%v failed=%v calls=%d reads=%d", done, failed, model.count.Load(), p.count("find"))
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, call := range p.calls {
		if call.method == "find" && call.request.(*statementv1.FindOperationsRequest).Filter.GetQ() != phone {
			t.Fatal("local financial search did not restore declared q")
		}
	}
	var stored string
	if err = env.App.DB.Reader(t.Context()).QueryRow(t.Context(), `SELECT text FROM question WHERE user_id=$1 AND id=$2`, generationAPIUser, accepted.Question.Id).Scan(&stored); err != nil || stored != question {
		t.Fatal("original question mutated", err)
	}
}

func TestAPIPrivacyFilterFailureNeverFallsBackToOriginalPayload(t *testing.T) {
	response := presenterAnalytics(t)
	nested := any("private@example.test")
	for range 70 {
		nested = map[string]any{"nested": nested}
	}
	data, err := structpb.NewStruct(map[string]any{"text": "ordinary text", "extension": nested})
	if err != nil {
		t.Fatal(err)
	}
	response.Blocks = []*commonv1.Block{{Type: "text", V: 1, Data: data}}
	peers := &generationAPIPeers{grant: true, analyticsResponse: response}
	model := generationAPIModel(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, generationAPIFunction("get_analytics", `{}`))
	})
	env := generationAPIEnv(t, peers, model)
	accepted := generationAPIAsk(t, env, generationAPIUser, generationAPIKey(), "Объясни мои расходы", commonv1.TransferMode_TRANSFER_MODE_WITHOUT)
	presenterAssertFailure(t, env, generationAPIUser, accepted.AnswerId)
	if peers.count("analytics") != 1 || model.count.Load() != 1 {
		t.Fatal("filter failure caused raw fallback or paid retry")
	}
}
