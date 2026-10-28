package app_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	assistantv1 "github.com/fin-assist/contracts/gen/go/finassist/assistant/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	statementv1 "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/fin-assist/assistant/internal/config"
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

func TestAPIPrivacyPreservesVisibleReasoningAndRefusesEncryptedContinuation(t *testing.T) {
	for _, mode := range []string{"visible", "opaque-only", "opaque-with-summary"} {
		t.Run(mode, func(t *testing.T) {
			peers := &generationAPIPeers{grant: true}
			var model *generationAPIHTTP
			model = generationAPIModel(t, func(step int, w http.ResponseWriter, _ *http.Request) {
				if step == 1 {
					events := providerWireEvents(t, generationAPIFunction("get_data_overview", `{}`))
					reason := map[string]any{"id": "reasoning-item", "type": "reasoning", "status": "completed"}
					if mode != "opaque-only" {
						reason["summary"] = []any{map[string]any{"type": "summary_text", "text": "thought@example.test"}}
					}
					if mode != "visible" {
						reason["encrypted_content"] = "opaque-provider-context"
					}
					for _, event := range events {
						if _, ok := event["output_index"]; ok {
							event["output_index"] = 1
						}
						if response, ok := event["response"].(map[string]any); ok {
							if output, ok := response["output"].([]any); ok && len(output) > 0 {
								response["output"] = append([]any{reason}, output...)
							}
						}
					}
					events = append([]map[string]any{events[0],
						{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"id": "reasoning-item", "type": "reasoning"}},
						{"type": "response.output_item.done", "output_index": 0, "item": reason}}, events[1:]...)
					_, _ = io.WriteString(w, generationAPIFrames(events...))
					return
				}
				model.mu.Lock()
				input := string(model.requests[step-1].body["input"])
				model.mu.Unlock()
				if !strings.Contains(input, "reasoning-item") || strings.Contains(input, "thought@example.test") || strings.Contains(input, "opaque-provider-context") {
					t.Error("reasoning continuation lost or unsafe")
				}
				_, _ = io.WriteString(w, generationAPIText("Финансовый ответ"))
			})
			env := generationAPIEnv(t, peers, model)
			accepted := generationAPIAsk(t, env, generationAPIUser, generationAPIKey(), "Объясни мои расходы", commonv1.TransferMode_TRANSFER_MODE_WITHOUT)
			if mode != "visible" {
				presenterAssertFailure(t, env, generationAPIUser, accepted.AnswerId)
				if model.count.Load() != 1 {
					t.Fatal("encrypted context was silently dropped and resent")
				}
				return
			}
			done, failed := generationAPITerminal(t, env, generationAPIUser, accepted.AnswerId)
			if done == nil || failed != nil || model.count.Load() != 2 {
				t.Fatalf("visible reasoning failed: %v %v calls=%d", done, failed, model.count.Load())
			}
		})
	}
}

func TestAPIPrivacyKnownBareRequisitesNeverEscapeEchoedArgumentsOrFilters(t *testing.T) {
	for _, tc := range []struct{ label, value string }{{"карта ", "4111111111111111"}, {"телефон: ", "79991234567"}, {"счёт ", "40817810099910004312"}} {
		t.Run(tc.label, func(t *testing.T) {
			response := presenterAggregate()
			response.Groups[0].Filters.Q = &tc.value
			amount := int64(9007199254740993)
			if parsed, err := strconv.ParseInt(tc.value, 10, 64); err == nil {
				amount = parsed
			}
			response.Groups[0].AmountMinor = amount
			response.TotalAmountMinor = amount
			peers := &generationAPIPeers{grant: true, aggregateResponse: response}
			var model *generationAPIHTTP
			var token string
			model = generationAPIModel(t, func(step int, w http.ResponseWriter, _ *http.Request) {
				model.mu.Lock()
				input := append(json.RawMessage(nil), model.requests[step-1].body["input"]...)
				model.mu.Unlock()
				var items []map[string]any
				_ = json.Unmarshal(input, &items)
				if step == 1 {
					tokens := regexp.MustCompile(`\[(?:phone|card|account)_[0-9a-f]{24}\]`).FindAllString(string(input), -1)
					if len(tokens) != 1 {
						t.Error("initial labelled requisite not masked")
						return
					}
					token = tokens[0]
					args, _ := json.Marshal(map[string]any{"filter": map[string]any{"q": token}, "group_by": []string{"category"}})
					_, _ = io.WriteString(w, generationAPIFunction("aggregate_operations", string(args)))
					return
				}
				for _, item := range items {
					if item["type"] == "function_call" {
						var args map[string]any
						_ = json.Unmarshal([]byte(item["arguments"].(string)), &args)
						if args["filter"].(map[string]any)["q"] != token {
							t.Error("bare restored arguments leaked on second POST")
						}
					}
					if item["type"] == "function_call_output" {
						var result map[string]any
						_ = json.Unmarshal([]byte(item["output"].(string)), &result)
						group := result["groups"].([]any)[0].(map[string]any)
						if group["filters"].(map[string]any)["q"] != token || group["amount_minor"] != strconv.FormatInt(amount, 10) || result["total_amount_minor"] != strconv.FormatInt(amount, 10) {
							t.Error("echo leak or quoted-money corruption on second POST")
						}
					}
				}
				_, _ = io.WriteString(w, generationAPIText("Финансовый ответ"))
			})
			env := generationAPIEnv(t, peers, model)
			accepted := generationAPIAsk(t, env, generationAPIUser, generationAPIKey(), tc.label+tc.value, commonv1.TransferMode_TRANSFER_MODE_WITHOUT)
			done, failed := generationAPITerminal(t, env, generationAPIUser, accepted.AnswerId)
			if done == nil || failed != nil || model.count.Load() != 2 || peers.count("aggregate") != 1 {
				t.Fatalf("bare echo scenario failed: %v %v", done, failed)
			}
		})
	}
}
func TestAPIPrivacyAliasesRestoreBeforeDisplayPersistenceAndNextHistory(t *testing.T) {
	const phone = "+7 (999) 123-45-67"
	const unknown = "[phone_000000000000000000000000]"
	var model *generationAPIHTTP
	var firstToken string
	model = generationAPIModel(t, func(step int, w http.ResponseWriter, _ *http.Request) {
		model.mu.Lock()
		input := string(model.requests[step-1].body["input"])
		model.mu.Unlock()
		tokens := regexp.MustCompile(`\[phone_[0-9a-f]{24}\]`).FindAllString(input, -1)
		if strings.Contains(input, phone) || len(tokens) == 0 {
			t.Error("unsafe/missing alias in followup HTTP")
			return
		}
		if step == 1 {
			firstToken = tokens[0]
			text := "На номер " + firstToken + " — 100 рублей; " + unknown
			events := providerWireEvents(t, generationAPIText(text))
			var split []map[string]any
			for _, event := range events {
				if event["type"] != "response.output_text.delta" {
					split = append(split, event)
					continue
				}
				for _, fragment := range []string{"На номер " + firstToken[:4], firstToken[4:15], firstToken[15:] + " — 100 рублей; " + unknown} {
					split = append(split, map[string]any{"type": "response.output_text.delta", "item_id": "m", "output_index": 0, "content_index": 0, "delta": fragment})
				}
			}
			_, _ = io.WriteString(w, generationAPIFrames(split...))
			return
		}
		known := []string{}
		for _, token := range tokens {
			if token != unknown {
				known = append(known, token)
			}
		}
		if len(known) != 2 || known[0] != known[1] || known[0] == firstToken || strings.Contains(input, firstToken) {
			t.Error("followup history aliases lost relation or reused prior scope")
		}
		_, _ = io.WriteString(w, generationAPIText("Уточнение"))
	})
	env := generationAPIEnv(t, &generationAPIPeers{grant: true}, model)
	first := generationAPIAsk(t, env, generationAPIUser, generationAPIKey(), "Покажи расходы по "+phone, commonv1.TransferMode_TRANSFER_MODE_WITHOUT)
	done, failed := generationAPITerminal(t, env, generationAPIUser, first.AnswerId)
	if done == nil || failed != nil {
		t.Fatal("restored streamed message did not match final raw", done, failed)
	}
	saved := presenterHistory(t, env, generationAPIUser)
	want := "На номер " + phone + " — 100 рублей; " + unknown
	if len(saved.Blocks) != 1 || saved.Blocks[0].GetText().GetText() != want {
		t.Fatal("user/history saw internal alias", saved)
	}
	second := generationAPIAsk(t, env, generationAPIUser, generationAPIKey(), "Уточни предыдущий ответ", commonv1.TransferMode_TRANSFER_MODE_WITHOUT)
	done, failed = generationAPITerminal(t, env, generationAPIUser, second.AnswerId)
	if done == nil || failed != nil || model.count.Load() != 2 {
		t.Fatal("followup failed", done, failed)
	}
}
func TestAPIPrivacyRestorationDoesNotCancelOffTopicPriority(t *testing.T) {
	for _, text := range []string{"Расскажи анекдот", "Расскажи анекдот про телефон +7 (999) 123-45-67"} {
		t.Run(text, func(t *testing.T) {
			peers := &generationAPIPeers{grant: true}
			model := generationAPIModel(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
				events := providerWireEvents(t, offTopicAPIBatch(true))
				for _, event := range events {
					if event["item_id"] == "read" {
						if _, ok := event["delta"]; ok {
							event["delta"] = `{"filter":{"q":7}}`
						}
						if _, ok := event["arguments"]; ok {
							event["arguments"] = `{"filter":{"q":7}}`
							event["name"] = "find_operations"
						}
					}
					if item, ok := event["item"].(map[string]any); ok && item["id"] == "read" {
						item["name"] = "find_operations"
						if item["arguments"] != "" {
							item["arguments"] = `{"filter":{"q":7}}`
						}
					}
					if response, ok := event["response"].(map[string]any); ok {
						if items, ok := response["output"].([]any); ok {
							for _, raw := range items {
								item := raw.(map[string]any)
								if item["id"] == "read" {
									item["name"] = "find_operations"
									item["arguments"] = `{"filter":{"q":7}}`
								}
							}
						}
					}
				}
				_, _ = io.WriteString(w, generationAPIFrames(events...))
			})
			env := offTopicAPIEnv(t, peers, model)
			accepted := generationAPIAsk(t, env, generationAPIUser, generationAPIKey(), text, commonv1.TransferMode_TRANSFER_MODE_WITHOUT)
			offTopicAPIAssertFailure(t, env, generationAPIUser, accepted.AnswerId)
			if offTopicAPICount(t, env, generationAPIUser) != 1 || model.count.Load() != 1 || peers.count("find")+peers.count("overview") != 0 {
				t.Fatal("OFF_TOPIC lost precedence or terminal accounting")
			}
		})
	}
}

func TestAPIPrivacyToolOnlyRequisiteStaysRecognizableInHistoryAndExactInSearch(t *testing.T) {
	for _, tc := range []struct{ label, value string }{{"карта ", "4111111111111111"}, {"телефон ", "79991234567"}, {"счёт ", "40817810099910004312"}} {
		t.Run(tc.label, func(t *testing.T) {
			description := tc.label + tc.value
			peers := &generationAPIPeers{grant: true, findResponse: &statementv1.FindOperationsResponse{
				Items:      []*statementv1.Operation{{Id: "10000000-0000-4000-8000-000000000001", Kind: statementv1.OperationKind_OPERATION_KIND_EXPENSE, OccurredAt: generationAPIRange().Start, AmountMinor: 100, Currency: "RUB", Title: "Перевод", Description: &description, Status: statementv1.OperationStatus_OPERATION_STATUS_POSTED}},
				TotalCount: 1, Range: generationAPIRange(), Bounds: generationAPIBounds(), TimeZone: "Europe/Moscow", Version: &statementv1.DataVersion{},
			}}
			var model *generationAPIHTTP
			var firstToken string
			model = generationAPIModel(t, func(step int, w http.ResponseWriter, _ *http.Request) {
				model.mu.Lock()
				input := string(model.requests[step-1].body["input"])
				model.mu.Unlock()
				if strings.Contains(input, tc.value) {
					t.Error("tool-only requisite leaked through history or repeated q")
				}
				if step == 1 {
					_, _ = io.WriteString(w, generationAPIFunction("find_operations", `{}`))
					return
				}
				tokens := regexp.MustCompile(`\[(?:phone|card|account)_[0-9a-f]{24}\]`).FindAllString(input, -1)
				if len(tokens) == 0 {
					t.Error("tool-only label was not recognized")
					return
				}
				if step == 2 {
					firstToken = tokens[0]
					text := "Реквизит: " + firstToken
					events := providerWireEvents(t, generationAPIText(text))
					var split []map[string]any
					for _, event := range events {
						if event["type"] != "response.output_text.delta" {
							split = append(split, event)
							continue
						}
						for _, fragment := range []string{"Реквизит: " + firstToken[:4], firstToken[4:15], firstToken[15:]} {
							split = append(split, map[string]any{"type": "response.output_text.delta", "item_id": "m", "output_index": 0, "content_index": 0, "delta": fragment})
						}
					}
					_, _ = io.WriteString(w, generationAPIFrames(split...))
					return
				}
				if step == 3 {
					if tokens[0] == firstToken || strings.Contains(input, firstToken) {
						t.Error("old alias reused in next history")
					}
					args, _ := json.Marshal(map[string]any{"filter": map[string]any{"q": tokens[0]}})
					_, _ = io.WriteString(w, generationAPIFunction("find_operations", string(args)))
					return
				}
				_, _ = io.WriteString(w, generationAPIText("Найдено"))
			})
			env := generationAPIEnv(t, peers, model)
			first := generationAPIAsk(t, env, generationAPIUser, generationAPIKey(), "Покажи переводы", commonv1.TransferMode_TRANSFER_MODE_WITHOUT)
			done, failed := generationAPITerminal(t, env, generationAPIUser, first.AnswerId)
			if done == nil || failed != nil {
				t.Fatal("tool-only echo failed", done, failed)
			}
			saved := presenterHistory(t, env, generationAPIUser)
			if len(saved.Blocks) != 1 || saved.Blocks[0].GetText().GetText() != "Реквизит: "+tc.label+tc.value {
				t.Fatal("display lost identifiable label or raw digits", saved)
			}
			followup := generationAPIAsk(t, env, generationAPIUser, generationAPIKey(), "Когда был этот перевод?", commonv1.TransferMode_TRANSFER_MODE_WITHOUT)
			done, failed = generationAPITerminal(t, env, generationAPIUser, followup.AnswerId)
			if done == nil || failed != nil || model.count.Load() != 4 || peers.count("find") != 2 {
				t.Fatal("tool-only followup failed", done, failed)
			}
			peers.mu.Lock()
			defer peers.mu.Unlock()
			reads := 0
			for _, call := range peers.calls {
				if call.method == "find" {
					reads++
					if reads == 2 && call.request.(*statementv1.FindOperationsRequest).Filter.GetQ() != tc.value {
						t.Fatal("followup q changed exact original requisite")
					}
				}
			}
		})
	}
}

func TestAPIPrivacyAmbiguousPANRefusesWithoutHTTPAndNaturalLabelsStayReadable(t *testing.T) {
	for _, text := range []string{"3000 4111 1111 1111 1111", "4111 1111 1111 1111 2", "3000 4111 1111 1111 1111 2", "4111 1111 1111 1111 00"} {
		t.Run(text, func(t *testing.T) {
			model := generationAPIModel(t, nil)
			env := generationAPIEnv(t, &generationAPIPeers{grant: true}, model)
			accepted := generationAPIAsk(t, env, generationAPIUser, generationAPIKey(), "Перевёл "+text, commonv1.TransferMode_TRANSFER_MODE_WITHOUT)
			presenterAssertFailure(t, env, generationAPIUser, accepted.AnswerId)
			if model.count.Load() != 0 {
				t.Fatal("ambiguous financial meaning sent to model")
			}
		})
	}
	for _, tc := range []struct{ label, prefix, value string }{{"карта ", "Списания по карте ", "4111111111111111"}, {"телефон ", "Переводы на телефон ", "79991234567"}, {"телефон ", "Переводы по телефону ", "79991234567"}, {"счёт ", "Движение по счёту ", "40817810099910004312"}} {
		t.Run(tc.prefix, func(t *testing.T) {
			var model *generationAPIHTTP
			model = generationAPIModel(t, func(step int, w http.ResponseWriter, _ *http.Request) {
				model.mu.Lock()
				input := string(model.requests[step-1].body["input"])
				model.mu.Unlock()
				tokens := regexp.MustCompile(`\[(?:phone|card|account)_[0-9a-f]{24}\]`).FindAllString(input, -1)
				if len(tokens) != 1 {
					t.Error("requisite not masked")
					return
				}
				token := tokens[0]
				text := tc.prefix + token
				events := providerWireEvents(t, generationAPIText(text))
				var split []map[string]any
				runes := []rune(tc.prefix)
				middle := len(runes) / 2
				for _, event := range events {
					if event["type"] != "response.output_text.delta" {
						split = append(split, event)
						continue
					}
					for _, fragment := range []string{string(runes[:middle]), string(runes[middle:]) + token[:4], token[4:]} {
						split = append(split, map[string]any{"type": "response.output_text.delta", "item_id": "m", "output_index": 0, "content_index": 0, "delta": fragment})
					}
				}
				_, _ = io.WriteString(w, generationAPIFrames(split...))
			})
			env := generationAPIEnv(t, &generationAPIPeers{grant: true}, model)
			accepted := generationAPIAsk(t, env, generationAPIUser, generationAPIKey(), tc.label+tc.value, commonv1.TransferMode_TRANSFER_MODE_WITHOUT)
			done, failed := generationAPITerminal(t, env, generationAPIUser, accepted.AnswerId)
			if done == nil || failed != nil {
				t.Fatal("natural label stream/final mismatch", done, failed)
			}
			saved := presenterHistory(t, env, generationAPIUser)
			if len(saved.Blocks) != 1 || saved.Blocks[0].GetText().GetText() != tc.prefix+tc.value {
				t.Fatal("visible/persisted label duplicated", saved)
			}
		})
	}
}

func TestAPIPrivacyHistoryTrimRetainsSessionKnowledgeAndOriginalDatabaseHistory(t *testing.T) {
	for _, tc := range []struct{ label, value string }{{"карта ", "4111111111111111"}, {"телефон ", "79991234567"}, {"счёт ", "40817810099910004312"}} {
		for _, mode := range []string{"budget", "transport"} {
			t.Run(tc.label+mode, func(t *testing.T) {
				response := presenterAggregate()
				selection := "s_merchant_private"
				response.Groups[0].Keys = []*statementv1.GroupKey{{Merchant: &statementv1.MerchantRef{Title: strings.Repeat("x", 20000), Selection: selection}}}
				response.Groups[0].Filters.Q = &tc.value
				response.Groups[0].Filters.CategoryId = nil
				response.Groups[0].Filters.Selection = &selection
				amount := int64(100)
				if value, err := strconv.ParseInt(tc.value, 10, 64); err == nil {
					amount = value
				}
				response.Groups[0].AmountMinor = amount
				response.Groups[0].AverageMinor = amount
				response.TotalAmountMinor = amount
				peers := &generationAPIPeers{grant: true, aggregateResponse: response, currentRevisions: strictAPIWire(1, 1, 1)}
				var model *generationAPIHTTP
				var token string
				model = generationAPIModel(t, func(step int, w http.ResponseWriter, _ *http.Request) {
					model.mu.Lock()
					input := append(json.RawMessage(nil), model.requests[step-1].body["input"]...)
					model.mu.Unlock()
					if step == 1 {
						aliases := regexp.MustCompile(`\[(?:phone|card|account)_[0-9a-f]{24}\]`).FindAllString(string(input), -1)
						if len(aliases) != 1 || !strings.Contains(string(input), "history-only-marker") {
							t.Error("seeded history not used or not masked")
							return
						}
						token = aliases[0]
						args, _ := json.Marshal(map[string]any{"filter": map[string]any{"q": token}, "group_by": []string{"merchant"}})
						_, _ = io.WriteString(w, generationAPIFunction("aggregate_operations", string(args)))
						return
					}
					if strings.Contains(string(input), "history-only-marker") {
						t.Error("old label pair was not trimmed")
					}
					var items []map[string]any
					_ = json.Unmarshal(input, &items)
					for _, item := range items {
						if item["type"] == "function_call" {
							var args map[string]any
							_ = json.Unmarshal([]byte(item["arguments"].(string)), &args)
							if args["filter"].(map[string]any)["q"] != token {
								t.Error("bare q leaked after source history trim")
							}
						}
						if item["type"] == "function_call_output" {
							var result map[string]any
							_ = json.Unmarshal([]byte(item["output"].(string)), &result)
							group := result["groups"].([]any)[0].(map[string]any)
							if group["filters"].(map[string]any)["q"] != token || group["amount_minor"] != strconv.FormatInt(amount, 10) || result["total_amount_minor"] != strconv.FormatInt(amount, 10) {
								t.Error("result q leaked or quoted money changed after history trim")
							}
						}
					}
					_, _ = io.WriteString(w, generationAPIText("Финансовый ответ"))
				})
				env := generationAPIEnv(t, peers, model, func(c *config.Config) { c.Generation.InputTokensPerStep = 9000; c.Generation.HistoryTokens = 9000 })
				const oldQuestion = "00000000-0000-4000-8000-000000000001"
				const oldAnswer = "00000000-0000-4000-8000-000000000002"
				history := tc.label + tc.value + " history-only-marker " + strings.Repeat("h", 20000)
				err := env.App.DB.Run(t.Context(), func(ctx context.Context) error {
					writer := env.App.DB.Writer(ctx)
					if _, err := writer.Exec(ctx, `INSERT INTO user_state(user_id,limit_day,limit_used,time_zone) VALUES ($1,'2026-10-23',0,'Europe/Moscow')`, generationAPIUser); err != nil {
						return err
					}
					if _, err := writer.Exec(ctx, `INSERT INTO question(user_id,id,text,transfer_mode,created_at) VALUES ($1,$2,'old generic question','without',$3)`, generationAPIUser, oldQuestion, generationAPINow.Add(-time.Hour)); err != nil {
						return err
					}
					if _, err := writer.Exec(ctx, `INSERT INTO answer(user_id,id,question_id,status,charge_day,charged,created_at) VALUES ($1,$2,$3,'complete','2026-10-23',false,$4)`, generationAPIUser, oldAnswer, oldQuestion, generationAPINow.Add(-time.Hour)); err != nil {
						return err
					}
					_, err := writer.Exec(ctx, `INSERT INTO answer_block(user_id,answer_id,position,type,schema_version,markdown) VALUES ($1,$2,0,'text',1,$3)`, generationAPIUser, oldAnswer, history)
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
				accepted := generationAPIAsk(t, env, generationAPIUser, generationAPIKey(), "Сколько всего ушло?", commonv1.TransferMode_TRANSFER_MODE_WITHOUT)
				done, failed := generationAPITerminal(t, env, generationAPIUser, accepted.AnswerId)
				if done == nil || failed != nil || model.count.Load() != 2 || peers.count("aggregate") != 1 {
					t.Fatal("successful post-trim continuation missing", done, failed, model.count.Load())
				}
				var saved string
				if err = env.App.DB.Writer(t.Context()).QueryRow(t.Context(), `SELECT markdown FROM answer_block WHERE user_id=$1 AND answer_id=$2`, generationAPIUser, oldAnswer).Scan(&saved); err != nil || saved != history {
					t.Fatal("original DB history changed", err)
				}
				peers.mu.Lock()
				defer peers.mu.Unlock()
				for _, call := range peers.calls {
					if call.method == "aggregate" && call.request.(*statementv1.AggregateOperationsRequest).Filter.GetQ() != tc.value {
						t.Fatal("local search lost exact original q")
					}
				}
			})
		}
	}
}
