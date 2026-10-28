package app_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	statementv1 "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"

	"github.com/fin-assist/assistant/internal/app/apitest"
)

const privacyLegacyAnswerID = "00000000-0000-4000-8000-000000000002"

func privacySeedHistory(t *testing.T, env *apitest.Env, text string) {
	t.Helper()
	err := env.App.DB.Run(t.Context(), func(ctx context.Context) error {
		writer := env.App.DB.Writer(ctx)
		if _, err := writer.Exec(ctx, `INSERT INTO user_state(user_id,limit_day,limit_used,time_zone) VALUES ($1,'2026-10-23',0,'Europe/Moscow')`, generationAPIUser); err != nil {
			return err
		}
		if _, err := writer.Exec(ctx, `INSERT INTO question(user_id,id,text,transfer_mode,created_at) VALUES ($1,'00000000-0000-4000-8000-000000000001','old generic question','without',$2)`, generationAPIUser, generationAPINow.Add(-time.Hour)); err != nil {
			return err
		}
		if _, err := writer.Exec(ctx, `INSERT INTO answer(user_id,id,question_id,status,charge_day,charged,created_at) VALUES ($1,$2,'00000000-0000-4000-8000-000000000001','complete','2026-10-23',false,$3)`, generationAPIUser, privacyLegacyAnswerID, generationAPINow.Add(-time.Hour)); err != nil {
			return err
		}
		_, err := writer.Exec(ctx, `INSERT INTO answer_block(user_id,answer_id,position,type,schema_version,markdown) VALUES ($1,$2,0,'text',1,$3)`, generationAPIUser, privacyLegacyAnswerID, text)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func privacyTextAndFunction(t *testing.T, text, name, args string) string {
	t.Helper()
	message := providerWireEvents(t, generationAPIText(text))
	function := providerWireEvents(t, generationAPIFunction(name, args))
	messageItem := message[len(message)-1]["response"].(map[string]any)["output"].([]any)[0]
	events := append([]map[string]any(nil), message[:len(message)-1]...)
	for _, event := range function[1:] {
		if _, ok := event["output_index"]; ok {
			event["output_index"] = 1
		}
		if response, ok := event["response"].(map[string]any); ok {
			response["output"] = append([]any{messageItem}, response["output"].([]any)...)
		}
		events = append(events, event)
	}
	return generationAPIFrames(events...)
}

func TestAPIPrivacyLegacyAmbiguousHistoryCanBeTrimmedWithoutBypassingCurrentInput(t *testing.T) {
	const ambiguous = "Карта 4111 1111 1111 1111 - 1000 ₽"
	for _, mode := range []string{"legacy", "question", "continuation"} {
		t.Run(mode, func(t *testing.T) {
			var model *generationAPIHTTP
			model = generationAPIModel(t, func(step int, w http.ResponseWriter, _ *http.Request) {
				model.mu.Lock()
				raw := string(model.requests[step-1].body["input"])
				model.mu.Unlock()
				if strings.Contains(raw, "4111 1111") {
					t.Error("ambiguous PAN reached HTTP")
				}
				if mode == "legacy" && strings.Contains(raw, "legacy-source-marker") {
					t.Error("unsafe legacy pair not trimmed")
				}
				if mode == "continuation" {
					_, _ = io.WriteString(w, privacyTextAndFunction(t, ambiguous, "get_data_overview", `{}`))
					return
				}
				_, _ = io.WriteString(w, generationAPIText("Финансовый ответ"))
			})
			env := generationAPIEnv(t, &generationAPIPeers{grant: true}, model)
			history, question := "Хорошая история legacy-source-marker", "Новый финансовый вопрос"
			if mode == "legacy" {
				history = ambiguous + " legacy-source-marker"
			}
			if mode == "question" {
				question = ambiguous
			}
			privacySeedHistory(t, env, history)
			accepted := generationAPIAsk(t, env, generationAPIUser, generationAPIKey(), question, commonv1.TransferMode_TRANSFER_MODE_WITHOUT)
			if mode == "legacy" {
				done, failed := generationAPITerminal(t, env, generationAPIUser, accepted.AnswerId)
				if done == nil || failed != nil || model.count.Load() != 1 {
					t.Fatal("legacy history blocked new question", done, failed, model.count.Load())
				}
			} else {
				presenterAssertFailure(t, env, generationAPIUser, accepted.AnswerId)
				want := int32(0)
				if mode == "continuation" {
					want = 1
				}
				if model.count.Load() != want {
					t.Fatal("current ambiguity bypassed after history trim", model.count.Load())
				}
			}
			var saved string
			if err := env.App.DB.Writer(t.Context()).QueryRow(t.Context(), `SELECT markdown FROM answer_block WHERE user_id=$1 AND answer_id=$2`, generationAPIUser, privacyLegacyAnswerID).Scan(&saved); err != nil || saved != history {
				t.Fatal("original history changed", err)
			}
		})
	}
}

func TestAPIPrivacyFormattedCardEchoKeepsMoneyAndExactSearchAcrossContinuationAndHistory(t *testing.T) {
	const value = "4111 1111 1111 1111"
	var model *generationAPIHTTP
	model = generationAPIModel(t, func(step int, w http.ResponseWriter, _ *http.Request) {
		model.mu.Lock()
		raw := append(json.RawMessage(nil), model.requests[step-1].body["input"]...)
		model.mu.Unlock()
		if strings.Contains(string(raw), value) {
			t.Error("full PAN reached HTTP")
		}
		aliases := regexp.MustCompile(`\[card_[0-9a-f]{24}\]`).FindAllString(string(raw), -1)
		if len(aliases) == 0 {
			t.Error("PAN correlation lost")
			return
		}
		token := aliases[0]
		if step == 1 || step == 3 {
			args, _ := json.Marshal(map[string]any{"filter": map[string]any{"q": token}})
			if step == 1 {
				_, _ = io.WriteString(w, privacyTextAndFunction(t, "Карта "+token+" - 1000 ₽", "find_operations", string(args)))
			} else {
				_, _ = io.WriteString(w, generationAPIFunction("find_operations", string(args)))
			}
			return
		}
		if !strings.Contains(string(raw), "1000") || !strings.Contains(string(raw), "«") {
			t.Error("money or visible PAN boundary lost")
		}
		_, _ = io.WriteString(w, generationAPIText("Готово"))
	})
	peers := &generationAPIPeers{grant: true}
	env := generationAPIEnv(t, peers, model)
	first := generationAPIAsk(t, env, generationAPIUser, generationAPIKey(), "Покажи карту "+value, commonv1.TransferMode_TRANSFER_MODE_WITHOUT)
	done, failed := generationAPITerminal(t, env, generationAPIUser, first.AnswerId)
	if done == nil || failed != nil || model.count.Load() != 2 {
		t.Fatal("restored continuation became ambiguous", done, failed, model.count.Load())
	}
	var text string
	if err := env.App.DB.Writer(t.Context()).QueryRow(t.Context(), `SELECT markdown FROM answer_block WHERE user_id=$1 AND answer_id=$2 AND position=0`, generationAPIUser, first.AnswerId).Scan(&text); err != nil || !strings.Contains(text, "Карта «"+value+"» - 1000 ₽") {
		t.Fatal("visible card boundary/money missing", err)
	}
	second := generationAPIAsk(t, env, generationAPIUser, generationAPIKey(), "Сколько по этой карте?", commonv1.TransferMode_TRANSFER_MODE_WITHOUT)
	done, failed = generationAPITerminal(t, env, generationAPIUser, second.AnswerId)
	if done == nil || failed != nil || model.count.Load() != 4 {
		t.Fatal("restored history blocked followup", done, failed, model.count.Load())
	}
	peers.mu.Lock()
	defer peers.mu.Unlock()
	count := 0
	for _, call := range peers.calls {
		if call.method == "find" {
			count++
			if call.request.(*statementv1.FindOperationsRequest).Filter.GetQ() != value {
				t.Error("display quotes entered local search")
			}
		}
	}
	if count != 2 {
		t.Fatal("local search not exercised twice", count)
	}
}
