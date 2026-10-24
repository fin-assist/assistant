package app_test

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"

	assistantv1 "github.com/fin-assist/contracts/gen/go/finassist/assistant/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	statementv1 "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestAPIPresentationCatalogValuesPreserveCurrencyAndDisplayMeaning(t *testing.T) {
	for _, tc := range []struct {
		name, currency, kind, data, path, text string
	}{
		{"foreign-amount", "USD", "rows", `{"rows":[{"label":"Возврат","value_kind":"amount","value":750}]}`, "blocks[0]", "7,50 USD"},
		{"percent-outside-unit-range", "RUB", "rows", `{"rows":[{"label":"Рост","value_kind":"percent","value":1.25}]}`, "blocks[0]", "1.25"},
		{"unknown-value-text", "RUB", "rows", `{"rows":[{"label":"Показатель","value_kind":"future","text":"Будущий формат"}]}`, "blocks[0]", "Будущий формат"},
		{"zero-count", "RUB", "rows", `{"rows":[{"label":"Покупок","value_kind":"count","value":0}]}`, "blocks[0]", ""},
		{"approximate-percent", "RUB", "tile_group", `{"tiles":[{"id":"metric","title":"Доля","value_kind":"percent","value":0.25,"status":"tentative","approx":true,"show_plus":true}]}`, "blocks[0].data.tiles[0]", "≈+0.25 (доля)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := presenterAnalytics(t)
			response.Currency = tc.currency
			response.Blocks = []*commonv1.Block{{Id: "values", Type: tc.kind, V: 1, Data: coverageAPIData(t, tc.data)}}
			saved := coverageAPIPresentation(t, response, `{"period":"month","period_key":"2026-09"}`, tc.path, false)
			rows := saved.Blocks[0].GetRows().GetRows()
			if len(rows) != 1 || rows[0].GetText() != tc.text {
				t.Fatalf("currency, percent meaning or explicit fallback lost: %v", rows)
			}
			if tc.name == "zero-count" {
				field := rows[0].ProtoReflect().Descriptor().Fields().ByName("count")
				if !rows[0].ProtoReflect().Has(field) || rows[0].GetCount() != 0 {
					t.Fatalf("known zero became a missing value: %v", rows[0])
				}
			}
		})
	}
}

func TestAPIPresentationOperationKindsAndEmptySelectionHaveTrustedLabels(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(map[bool]string{false: "income-and-own-transfer", true: "known-empty-selection"}[empty], func(t *testing.T) {
			peer := &generationAPIPeers{grant: true, findResponse: &statementv1.FindOperationsResponse{Range: generationAPIRange(), Bounds: generationAPIBounds(), TimeZone: "Europe/Moscow", Version: &statementv1.DataVersion{}, TotalCount: 2, Items: []*statementv1.Operation{
				{Id: "30000000-0000-4000-8000-000000000021", Kind: statementv1.OperationKind_OPERATION_KIND_INCOME, Status: statementv1.OperationStatus_OPERATION_STATUS_POSTED, Title: "Возврат", AmountMinor: 0, Currency: "RUB", OccurredAt: generationAPIRange().Start},
				{Id: "30000000-0000-4000-8000-000000000022", Kind: statementv1.OperationKind_OPERATION_KIND_OWN_TRANSFER, Status: statementv1.OperationStatus_OPERATION_STATUS_POSTED, Title: "Между счетами", AmountMinor: 100, Currency: "RUB", OccurredAt: generationAPIRange().Start},
			}}}
			if empty {
				peer.findResponse.Items, peer.findResponse.TotalCount = nil, 0
			}
			model := generationAPIModel(t, func(step int, w http.ResponseWriter, _ *http.Request) {
				switch step {
				case 1:
					_, _ = io.WriteString(w, generationAPIFunctionID("read", "find_operations", `{"all_time":true}`))
				case 2:
					_, _ = io.WriteString(w, generationAPIFunctionID("show", "present_result", `{"result_id":"read","view":"rows"}`))
				default:
					_, _ = io.WriteString(w, generationAPIText("Данные показаны"))
				}
			})
			env := generationAPIEnv(t, peer, model)
			accepted := generationAPIAsk(t, env, generationAPIUser, generationAPIKey(), "Покажи найденное", commonv1.TransferMode_TRANSFER_MODE_WITH)
			done, failure := generationAPITerminal(t, env, generationAPIUser, accepted.AnswerId)
			if done == nil || failure != nil || !done.Charged {
				t.Fatalf("known empty/transfer selection was treated as missing data: %v %v", done, failure)
			}
			saved := presenterHistory(t, env, generationAPIUser)
			rows := saved.Blocks[0].GetRows().GetRows()
			if len(rows) == 0 || rows[0].GetCount() != int64(peer.findResponse.TotalCount) || len(saved.Chips) != 1 || saved.Chips[0].GetScreen() != assistantv1.ChipScreen_CHIP_SCREEN_OPERATIONS || saved.Chips[0].GetFilters().GetTransferMode() != commonv1.TransferMode_TRANSFER_MODE_WITH {
				t.Fatalf("selection count or caller transfer mode lost: %v", saved)
			}
			if !empty && (len(rows) != 3 || rows[1].GetLabel() != "Доход: Возврат" || rows[2].GetLabel() != "Перевод: Между счетами") {
				t.Fatalf("operation kinds were reinterpreted: %v", rows)
			}
		})
	}
}

func TestAPIPresentationAggregateDimensionsAndExactOverflowSurviveReconnect(t *testing.T) {
	for _, variant := range []string{"merchant-month", "unbounded-total", "ungrouped-total", "missing-group-filter", "different-dimension"} {
		t.Run(variant, func(t *testing.T) {
			response := presenterAggregate()
			response.TotalAmountMinor, response.TotalOperationCount = math.MaxInt64, math.MaxInt64
			group := response.Groups[0]
			group.AmountMinor, group.AverageMinor, group.OperationCount = math.MaxInt64, math.MaxInt64, math.MaxInt64
			month := "2026-09"
			group.Keys = append([]*statementv1.GroupKey{}, &statementv1.GroupKey{Merchant: &statementv1.MerchantRef{Title: "Кофейни", Selection: "s_coffee"}}, &statementv1.GroupKey{Month: &month})
			response.Truncated = true
			read := `{"all_time":true,"group_by":["merchant","month"]}`
			switch variant {
			case "unbounded-total":
				response.Groups = nil
				read = `{"all_time":true,"filter":{"q":"Кофе"}}`
			case "ungrouped-total":
				group.Keys = nil
				read = `{"all_time":true}`
			case "missing-group-filter":
				group.Filters = nil
			case "different-dimension":
				read = `{"all_time":true,"group_by":["month","merchant"]}`
			}
			peer := &generationAPIPeers{grant: true, aggregateResponse: response}
			model := generationAPIModel(t, func(step int, w http.ResponseWriter, _ *http.Request) {
				switch step {
				case 1:
					_, _ = io.WriteString(w, generationAPIFunctionID("read", "aggregate_operations", read))
				case 2:
					_, _ = io.WriteString(w, generationAPIFunctionID("show", "present_result", `{"result_id":"read","view":"rows"}`))
				default:
					_, _ = io.WriteString(w, generationAPIText("Показана точная сумма"))
				}
			})
			env := generationAPIEnv(t, peer, model)
			accepted := generationAPIAsk(t, env, generationAPIUser, generationAPIKey(), "Покажи точные расходы", commonv1.TransferMode_TRANSFER_MODE_WITHOUT)
			if variant == "missing-group-filter" || variant == "different-dimension" {
				presenterAssertFailure(t, env, generationAPIUser, accepted.AnswerId)
				return
			}
			done, failure := generationAPITerminal(t, env, generationAPIUser, accepted.AnswerId)
			if done == nil || failure != nil || !done.Charged || done.RemainingLimit != 4 {
				t.Fatalf("aggregate terminal: %v %v", done, failure)
			}
			saved := presenterHistory(t, env, generationAPIUser)
			snapshot := presenterSnapshot(t, env, generationAPIUser, accepted.AnswerId)
			if len(saved.Blocks) != 2 || saved.Blocks[0].GetRows() == nil || !proto.Equal(saved.Blocks[0], snapshot.Blocks[0]) || len(saved.Chips) != 1 {
				t.Fatalf("exact rows changed across storage/reconnect: %v %v", saved, snapshot)
			}
			rows := saved.Blocks[0].GetRows().GetRows()
			if rows[0].GetText() != "92233720368547758,07 ₽" || rows[1].GetText() != "9223372036854775807" || !strings.Contains(rows[len(rows)-1].GetText(), "часть групп") {
				t.Fatalf("overflow was rounded or truncation hidden: %v", rows)
			}
			if variant == "merchant-month" && rows[2].GetLabel() != "Кофейни · 2026-09" {
				t.Fatalf("multiple grouping dimensions lost: %v", rows[2])
			}
			if variant == "ungrouped-total" && rows[2].GetLabel() != "Итого по выборке" {
				t.Fatalf("ungrouped result was mislabeled: %v", rows[2])
			}
		})
	}
}

func TestAPIPresentationMonthlyCoverageCannotBeInventedByModel(t *testing.T) {
	for _, variant := range []string{"partial-gaps-truncated", "exact-overflow-fallback", "missing-month", "duplicate-month", "duplicate-group", "wrong-month-range", "partial-calendar", "no-data-group", "outside-data-bounds", "outside-gap", "missing-coverage-reference"} {
		t.Run(variant, func(t *testing.T) {
			peer := presenterMonthlyPeers()
			month := peer.overviewResponse.Months[1]
			present := `{"result_id":"aggregate","view":"chart","coverage_result_id":"coverage"}`
			switch variant {
			case "partial-gaps-truncated":
				month.Bounds.Coverage = commonv1.Coverage_COVERAGE_PARTIAL
				month.Bounds.DataFrom = timestamppb.New(month.Range.Start.AsTime().AddDate(0, 0, 2))
				month.Bounds.Gaps = []*commonv1.TimeRange{{Start: timestamppb.New(month.Range.Start.AsTime().AddDate(0, 0, 10)), End: timestamppb.New(month.Range.Start.AsTime().AddDate(0, 0, 11))}}
				peer.aggregateResponse.Truncated = true
			case "exact-overflow-fallback":
				peer.aggregateResponse.Groups[0].AmountMinor = math.MaxInt64
			case "missing-month":
				peer.overviewResponse.Months = peer.overviewResponse.Months[:1]
			case "duplicate-month":
				peer.overviewResponse.Months = append(peer.overviewResponse.Months, proto.Clone(month).(*statementv1.MonthCoverage))
			case "duplicate-group":
				peer.aggregateResponse.Groups = append(peer.aggregateResponse.Groups, proto.Clone(peer.aggregateResponse.Groups[0]).(*statementv1.AggregateGroup))
			case "wrong-month-range":
				month.Range.End = timestamppb.New(month.Range.End.AsTime().AddDate(0, 1, 0))
			case "partial-calendar":
				peer.aggregateResponse.Range.Start = timestamppb.New(peer.aggregateResponse.Range.Start.AsTime().AddDate(0, 0, 1))
			case "no-data-group":
				month.Bounds = &commonv1.DataBounds{Coverage: commonv1.Coverage_COVERAGE_NO_DATA}
			case "outside-data-bounds":
				month.Bounds.DataFrom = timestamppb.New(month.Range.Start.AsTime().AddDate(0, 0, -1))
			case "outside-gap":
				month.Bounds.Gaps = []*commonv1.TimeRange{{Start: timestamppb.New(month.Range.Start.AsTime().AddDate(0, 0, -2)), End: timestamppb.New(month.Range.Start.AsTime().AddDate(0, 0, -1))}}
			case "missing-coverage-reference":
				present = `{"result_id":"aggregate","view":"chart","coverage_result_id":"another-question"}`
			}
			model := generationAPIModel(t, func(step int, w http.ResponseWriter, _ *http.Request) {
				switch step {
				case 1:
					_, _ = io.WriteString(w, generationAPIFunctionID("coverage", "get_data_overview", `{}`))
				case 2:
					_, _ = io.WriteString(w, generationAPIFunctionID("aggregate", "aggregate_operations", `{"all_time":true,"group_by":["month"]}`))
				case 3:
					_, _ = io.WriteString(w, generationAPIFunctionID("show", "present_result", present))
				default:
					_, _ = io.WriteString(w, generationAPIText("Месяцы показаны"))
				}
			})
			env := generationAPIEnv(t, peer, model)
			accepted := generationAPIAsk(t, env, generationAPIUser, generationAPIKey(), "Покажи данные по месяцам", commonv1.TransferMode_TRANSFER_MODE_WITHOUT)
			if variant != "partial-gaps-truncated" && variant != "exact-overflow-fallback" {
				presenterAssertFailure(t, env, generationAPIUser, accepted.AnswerId)
				return
			}
			done, failure := generationAPITerminal(t, env, generationAPIUser, accepted.AnswerId)
			if done == nil || failure != nil || !done.Charged {
				t.Fatalf("valid chart/fallback: %v %v", done, failure)
			}
			saved := presenterHistory(t, env, generationAPIUser)
			if variant == "exact-overflow-fallback" {
				if saved.Blocks[0].GetRows() == nil || saved.Blocks[0].GetRows().GetRows()[2].GetText() != "92233720368547758,07 ₽" {
					t.Fatalf("large amount was rounded into a chart: %v", saved)
				}
				return
			}
			chart := saved.Blocks[0].GetChart()
			if chart == nil || len(chart.Points) != 1 || chart.Points[0].GetBounds().GetCoverage() != commonv1.Coverage_COVERAGE_PARTIAL || !proto.Equal(chart.Points[0].GetBounds().GetDataFrom(), month.Bounds.DataFrom) || len(saved.Blocks) != 3 || !strings.Contains(saved.Blocks[1].GetText().GetText(), "нет данных с") || !strings.Contains(saved.Blocks[1].GetText().GetText(), "часть месяцев") {
				t.Fatalf("partial coverage or visible limitations were discarded: %v", saved)
			}
			// No absent August group may be fabricated as a zero-valued point.
			if !proto.Equal(chart.Points[0].GetRange(), month.Range) {
				t.Fatalf("absent group became a financial point: %v", chart)
			}
		})
	}
}

func TestAPIPresentationCitationURLPolicyClearsUnsafePartialAnswers(t *testing.T) {
	for _, address := range []string{"//example.org", "https://user:private@example.org", "https://example.org/%0Aprivate", "https://-bad.example", "https://example.org:65536", "https://example.org/white space"} {
		t.Run(address, func(t *testing.T) {
			model := generationAPIModel(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, presenterCitedText("Частичный ответ", address))
			})
			env := generationAPIEnv(t, &generationAPIPeers{grant: true}, model)
			accepted := generationAPIAsk(t, env, generationAPIUser, generationAPIKey(), "Проверь финансовые источники", commonv1.TransferMode_TRANSFER_MODE_WITHOUT)
			presenterAssertFailure(t, env, generationAPIUser, accepted.AnswerId)
			page := presenterHistory(t, env, generationAPIUser)
			encoded, err := json.Marshal(page)
			if err != nil || strings.Contains(string(encoded), address) || strings.Contains(string(encoded), "Частичный ответ") {
				t.Fatalf("untrusted citation survived failure cleanup: %s err=%v", encoded, err)
			}
		})
	}
}

func TestAPIPresentationCitationTitlesStayPlainAndOffsetsCannotForgeProof(t *testing.T) {
	for _, variant := range []string{"plain-cleaned-title", "oversized-title", "non-string-title", "negative-start", "reversed-offsets"} {
		t.Run(variant, func(t *testing.T) {
			stream := presenterCitedText("Сведения об источниках", "www.rbc.ru", "https://example.org/reference")
			title, err := json.Marshal("  <b>**Источник**</b>\n[данные]\t`проверено`\u202e\x00")
			if err != nil {
				t.Fatal(err)
			}
			switch variant {
			case "plain-cleaned-title":
				stream = strings.ReplaceAll(stream, `"title":""`, `"title":`+string(title))
			case "oversized-title":
				title, err = json.Marshal(strings.Repeat("я", 129))
				if err != nil {
					t.Fatal(err)
				}
				stream = strings.ReplaceAll(stream, `"title":""`, `"title":`+string(title))
			case "non-string-title":
				stream = strings.ReplaceAll(stream, `"title":""`, `"title":17`)
			case "negative-start":
				stream = strings.ReplaceAll(stream, `"start_index":0`, `"start_index":-1`)
			case "reversed-offsets":
				stream = strings.ReplaceAll(stream, `"start_index":0`, `"start_index":2`)
			}
			model := generationAPIModel(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, stream)
			})
			env := generationAPIEnv(t, &generationAPIPeers{grant: true}, model)
			accepted := generationAPIAsk(t, env, generationAPIUser, generationAPIKey(), "Подтверди финансовую справку", commonv1.TransferMode_TRANSFER_MODE_WITHOUT)
			if variant != "plain-cleaned-title" {
				presenterAssertFailure(t, env, generationAPIUser, accepted.AnswerId)
				return
			}
			done, failure := generationAPITerminal(t, env, generationAPIUser, accepted.AnswerId)
			if done == nil || failure != nil || !done.Charged {
				t.Fatalf("valid provider citations were refused: %v %v", done, failure)
			}
			saved := presenterHistory(t, env, generationAPIUser)
			if len(saved.Blocks) != 2 {
				t.Fatalf("confirmed citations were not durable: %v", saved)
			}
			text := saved.Blocks[1].GetText().GetText()
			if !strings.Contains(text, "Источник") || !strings.Contains(text, "https://www.rbc.ru") || !strings.Contains(text, "https://example.org/reference") || strings.Count(text, "\n") != 2 || strings.ContainsAny(text, "<>[]*`\t\u202e\x00") {
				t.Fatalf("provider title injected formatting/control characters or lost sources: %q", text)
			}
		})
	}
}
