package app_test

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	assistantv1 "github.com/fin-assist/contracts/gen/go/finassist/assistant/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	statementv1 "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/fin-assist/assistant/internal/app"
	"github.com/fin-assist/assistant/internal/app/apitest"
	"github.com/fin-assist/assistant/internal/config"
)

type coverageAPIPeriodPeer struct {
	*generationAPIPeers
	periods *analyticsv1.ListPeriodsResponse
	err     error
}

func (p *coverageAPIPeriodPeer) ListPeriods(ctx context.Context, request *analyticsv1.ListPeriodsRequest) (*analyticsv1.ListPeriodsResponse, error) {
	p.record(ctx, "periods", request)
	return p.periods, p.err
}

func coverageAPIPeriodEnv(t *testing.T, peer *coverageAPIPeriodPeer, model *generationAPIHTTP) *apitest.Env {
	t.Helper()
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	userv1.RegisterUserInternalServiceServer(server, peer)
	statementv1.RegisterAssistantToolsServiceServer(server, peer)
	analyticsv1.RegisterAnalyticsServiceServer(server, peer)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	options := app.Options{Clock: func() time.Time { return generationAPINow }, PeerDialOptions: map[string][]grpc.DialOption{}}
	for _, name := range []string{"user", "bank-statement-data", "analytics"} {
		options.PeerDialOptions[name] = []grpc.DialOption{grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) })}
	}
	key := filepath.Join(t.TempDir(), "model-key")
	if err := os.WriteFile(key, []byte("local-test-key"), 0600); err != nil {
		t.Fatal(err)
	}
	return apitest.StartWithOptions(t, options, func(c *config.Config) {
		for _, name := range []string{"user", "bank-statement-data", "analytics"} {
			c.Peers[name] = "passthrough:///" + name
		}
		c.Generation = config.GenerationConfig{ModelURI: "gpt://folder/aliceai-llm", FolderID: "folder", Endpoint: model.server.URL + "/v1/responses", APIKeyFile: key, InputTokensPerStep: 24000, InputTokensTotal: 48000, HistoryTokens: 16000, RequestBytesPerStep: 65536, RequestBytesTotal: 131072, MaxOutputTokens: 4096, MaxSteps: 8, MaxFunctions: 16, ParallelFunctions: 4, MaxSearchCalls: 3, FinancialResultBytes: 32768, FinancialCollectionItems: 200, ModelResponseBytes: 1048576, ModelEventBytes: 65536, ModelArgumentBytes: 16384, ModelOutputItems: 64}
	})
}

func TestAPICoveragePeriodsPeerFailuresRefundAndSuppressPrivateDependencyText(t *testing.T) {
	for _, code := range []codes.Code{codes.Unavailable, codes.DeadlineExceeded, codes.Canceled, codes.Internal} {
		t.Run(code.String(), func(t *testing.T) {
			peer := &coverageAPIPeriodPeer{generationAPIPeers: &generationAPIPeers{grant: true}, err: status.Error(code, "PRIVATE database host and user data")}
			model := generationAPIModel(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, generationAPIFunctionID("periods", "list_periods", `{"period":"month"}`))
			})
			env := coverageAPIPeriodEnv(t, peer, model)
			accepted := generationAPIAsk(t, env, generationAPIUser, generationAPIKey(), "Какие периоды доступны?", commonv1.TransferMode_TRANSFER_MODE_WITHOUT)
			presenterAssertFailure(t, env, generationAPIUser, accepted.AnswerId)
			if model.count.Load() != 1 {
				t.Fatal("a failed dependency reached a model continuation")
			}
			saved := presenterHistory(t, env, generationAPIUser)
			if strings.Contains(saved.String(), "PRIVATE") {
				t.Fatalf("private dependency text reached history: %v", saved)
			}
		})
	}
}

func TestAPICoveragePeriodsResponsesValidateCalendarAndKeepProviderOrder(t *testing.T) {
	for _, variant := range []string{"valid", "invalid-zone", "invalid-key", "reversed-range", "no-data-coverage", "unknown-enum"} {
		t.Run(variant, func(t *testing.T) {
			response := &analyticsv1.ListPeriodsResponse{TimeZone: "Europe/Moscow", Periods: []*analyticsv1.PeriodItem{
				{Key: "2026-10", Range: &commonv1.TimeRange{Start: timestamppb.New(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)), End: timestamppb.New(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))}, Coverage: commonv1.Coverage_COVERAGE_PARTIAL},
				{Key: "2026-09", Range: generationAPIRange(), Coverage: commonv1.Coverage_COVERAGE_COMPLETE},
			}}
			switch variant {
			case "invalid-zone":
				response.TimeZone = "Not/A_Zone"
			case "invalid-key":
				response.Periods[0].Key = "2026-15"
			case "reversed-range":
				response.Periods[0].Range.Start, response.Periods[0].Range.End = response.Periods[0].Range.End, response.Periods[0].Range.Start
			case "no-data-coverage":
				response.Periods[0].Coverage = commonv1.Coverage_COVERAGE_NO_DATA
			case "unknown-enum":
				response.Periods[0].Coverage = commonv1.Coverage(999)
			}
			peer := &coverageAPIPeriodPeer{generationAPIPeers: &generationAPIPeers{grant: true}, periods: response}
			model := generationAPIModel(t, func(step int, w http.ResponseWriter, _ *http.Request) {
				if step == 1 {
					_, _ = io.WriteString(w, generationAPIFunctionID("periods", "list_periods", `{"period":"month"}`))
				} else {
					_, _ = io.WriteString(w, generationAPIText("Октябрь неполный; сентябрь полный"))
				}
			})
			env := coverageAPIPeriodEnv(t, peer, model)
			accepted := generationAPIAsk(t, env, generationAPIUser, generationAPIKey(), "Сравни периоды", commonv1.TransferMode_TRANSFER_MODE_WITHOUT)
			if variant != "valid" {
				presenterAssertFailure(t, env, generationAPIUser, accepted.AnswerId)
				return
			}
			done, failure := generationAPITerminal(t, env, generationAPIUser, accepted.AnswerId)
			if failure != nil || done == nil || !done.Charged {
				t.Fatalf("valid periods: %v %v", done, failure)
			}
			model.mu.Lock()
			defer model.mu.Unlock()
			input := string(model.requests[1].body["input"])
			if !strings.Contains(input, "2026-10") || !strings.Contains(input, "2026-09") || strings.Index(input, "2026-10") > strings.Index(input, "2026-09") {
				t.Fatalf("periods reordered or omitted before model continuation: %s", input)
			}
		})
	}
}

func TestAPICoverageAvailablePeriodsAreAuxiliaryAndKeepCallerScope(t *testing.T) {
	for _, tc := range []struct {
		name, args string
		period     commonv1.PeriodType
	}{
		{"default", `{}`, commonv1.PeriodType_PERIOD_TYPE_MONTH},
		{"month", `{"period":"month"}`, commonv1.PeriodType_PERIOD_TYPE_MONTH},
		{"quarter", `{"period":"quarter"}`, commonv1.PeriodType_PERIOD_TYPE_QUARTER},
		{"year", `{"period":"year"}`, commonv1.PeriodType_PERIOD_TYPE_YEAR},
	} {
		t.Run(tc.name, func(t *testing.T) {
			peers := &generationAPIPeers{grant: true}
			model := generationAPIModel(t, func(step int, w http.ResponseWriter, _ *http.Request) {
				if step == 1 {
					_, _ = io.WriteString(w, generationAPIFunctionID("periods", "list_periods", tc.args))
				} else {
					_, _ = io.WriteString(w, generationAPIText("Периодов с данными пока нет"))
				}
			})
			env := generationAPIEnv(t, peers, model)
			accepted := generationAPIAsk(t, env, generationAPIUser, generationAPIKey(), "Какие периоды доступны?", commonv1.TransferMode_TRANSFER_MODE_WITHOUT)
			done, failed := generationAPITerminal(t, env, generationAPIUser, accepted.AnswerId)
			if failed != nil || done == nil || !done.Charged || done.RemainingLimit != 4 {
				t.Fatalf("auxiliary empty periods must not refund a complete answer: %v %v", done, failed)
			}
			saved := presenterHistory(t, env, generationAPIUser)
			if len(saved.Blocks) != 1 || saved.Blocks[0].GetText().GetText() != "Периодов с данными пока нет" || len(saved.Chips) != 0 {
				t.Fatalf("empty periods invented presentation/navigation: %v", saved)
			}
			peers.mu.Lock()
			defer peers.mu.Unlock()
			var calls int
			for _, call := range peers.calls {
				if call.method != "periods" {
					continue
				}
				calls++
				request := call.request.(*analyticsv1.ListPeriodsRequest)
				if request.GetUserId() != generationAPIUser || request.Period != tc.period || len(call.md.Get("authorization")) != 0 || len(call.md.Get("x-service-authorization")) != 1 || call.md.Get("x-service-authorization")[0] != "Bearer dev:apitest" {
					t.Fatalf("period selection or caller scope changed: %v %v", request, call.md)
				}
			}
			if calls != 1 || model.count.Load() != 2 {
				t.Fatalf("periods calls=%d model=%d", calls, model.count.Load())
			}
		})
	}
}

func coverageAPIPresentation(t *testing.T, response *analyticsv1.GetAnalyticsResponse, read, path string, failed bool, wantCharged ...bool) *assistantv1.Message {
	t.Helper()
	args, err := json.Marshal(map[string]string{"result_id": "analytics", "view": "analytics", "path": path})
	if err != nil {
		t.Fatal(err)
	}
	peers := &generationAPIPeers{grant: true, analyticsResponse: response}
	model := generationAPIModel(t, func(step int, w http.ResponseWriter, _ *http.Request) {
		switch step {
		case 1:
			_, _ = io.WriteString(w, generationAPIFunctionID("analytics", "get_analytics", read))
		case 2:
			_, _ = io.WriteString(w, generationAPIFunctionID("show", "present_result", string(args)))
		default:
			_, _ = io.WriteString(w, generationAPIText("Готово"))
		}
	})
	env := generationAPIEnv(t, peers, model)
	accepted := generationAPIAsk(t, env, generationAPIUser, generationAPIKey(), "Покажи данные", commonv1.TransferMode_TRANSFER_MODE_WITHOUT)
	if failed {
		presenterAssertFailure(t, env, generationAPIUser, accepted.AnswerId)
		return nil
	}
	done, failure := generationAPITerminal(t, env, generationAPIUser, accepted.AnswerId)
	charged, remaining := true, int32(4)
	if len(wantCharged) > 0 && !wantCharged[0] {
		charged, remaining = false, 5
	}
	if failure != nil || done == nil || done.Charged != charged || done.RemainingLimit != remaining {
		t.Fatalf("terminal=%v failure=%v", done, failure)
	}
	saved := presenterHistory(t, env, generationAPIUser)
	snapshot := presenterSnapshot(t, env, generationAPIUser, accepted.AnswerId)
	if len(saved.Blocks) != 2 || len(snapshot.Blocks) != 2 || !proto.Equal(saved.Blocks[0], snapshot.Blocks[0]) || len(saved.Chips) != len(snapshot.Chips) {
		t.Fatalf("presentation changed across history and reconnect: %v %v", saved, snapshot)
	}
	for i, chip := range saved.Chips {
		if !proto.Equal(chip, snapshot.Chips[i]) {
			t.Fatalf("chip changed on reconnect: %v %v", chip, snapshot.Chips[i])
		}
	}
	return saved
}

func coverageAPIData(t *testing.T, raw string) *structpb.Struct {
	t.Helper()
	var values map[string]any
	if err := json.Unmarshal([]byte(raw), &values); err != nil {
		t.Fatal(err)
	}
	data, err := structpb.NewStruct(values)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestAPICoverageCatalogNavigationPreservesTrustedFiltersAndRejectsUnsafeActions(t *testing.T) {
	for _, tc := range []struct {
		name, action string
		wantChip     bool
	}{
		{"complete-filter", `{"type":"open_operations","filter":{"from":"2026-09-01T00:00:00Z","to":"2026-10-01T00:00:00Z","q":"Кофе","category_id":"20000000-0000-4000-8000-000000000001","kind":"expense","transfer_mode":"without","selection":"s_coffee","selection_name":"Кофейни","amount_from":100,"amount_to":50000}}`, true},
		{"duplicate-actions", `{"type":"open_operations","filter":{"q":"Кофе","transfer_mode":"without"}}`, true},
		{"future-action", `{"type":"new_screen","filter":{"q":"Кофе","transfer_mode":"without"}}`, false},
		{"different-transfer-mode", `{"type":"open_operations","filter":{"q":"Кофе","transfer_mode":"with"}}`, false},
		{"unsupported-kind", `{"type":"open_operations","filter":{"kind":"own_transfer","transfer_mode":"without"}}`, false},
		{"empty-filter", `{"type":"open_operations","filter":{"transfer_mode":"without"}}`, false},
		{"selection-name-is-not-selection", `{"type":"open_operations","filter":{"selection_name":"Кофейни","transfer_mode":"without"}}`, false},
		{"reversed-range", `{"type":"open_operations","filter":{"from":"2026-10-01T00:00:00Z","to":"2026-09-01T00:00:00Z","transfer_mode":"without"}}`, false},
		{"negative-amount", `{"type":"open_operations","filter":{"amount_from":-1,"transfer_mode":"without"}}`, false},
		{"reversed-amounts", `{"type":"open_operations","filter":{"amount_from":1000,"amount_to":100,"transfer_mode":"without"}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := presenterAnalytics(t)
			response.Blocks = []*commonv1.Block{{Id: "rows", Type: "rows", V: 1, Data: coverageAPIData(t, `{"rows":[{"label":"Кофе","value_kind":"amount","value":12000,"action":`+tc.action+`},{"label":"Повтор","value_kind":"count","value":2,"action":`+tc.action+`}]}`)}}
			saved := coverageAPIPresentation(t, response, `{"period":"month","period_key":"2026-09"}`, "blocks[0]", false)
			wantCount := 1
			if tc.wantChip {
				wantCount++
			}
			if len(saved.Chips) != wantCount {
				t.Fatalf("unsafe or duplicate navigation: %v", saved.Chips)
			}
			if tc.wantChip {
				chip := saved.Chips[0]
				if chip.GetScreen() != assistantv1.ChipScreen_CHIP_SCREEN_OPERATIONS || chip.GetFilters().GetQ() != "Кофе" || chip.GetFilters().GetTransferMode() != commonv1.TransferMode_TRANSFER_MODE_WITHOUT {
					t.Fatalf("trusted operation navigation changed: %v", chip)
				}
				if tc.name == "complete-filter" && (chip.GetFilters().GetAmountFromMinor() != 100 || chip.GetFilters().GetAmountToMinor() != 50000 || chip.GetFilters().GetSelection() != "s_coffee" || chip.GetFilters().GetCategoryId() != presenterCategory || chip.GetFilters().GetKind() != commonv1.KindFilter_KIND_FILTER_EXPENSE) {
					t.Fatalf("filter bounds/selection lost: %v", chip)
				}
			}
		})
	}
}

func TestAPICoverageCatalogQuarterYearAndNativeBlockAction(t *testing.T) {
	for _, tc := range []struct {
		name, key, read string
		period          commonv1.PeriodType
	}{
		{"quarter", "2026-Q3", `{"period":"quarter","period_key":"2026-Q3"}`, commonv1.PeriodType_PERIOD_TYPE_QUARTER},
		{"year", "2026", `{"period":"year","period_key":"2026"}`, commonv1.PeriodType_PERIOD_TYPE_YEAR},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := presenterAnalytics(t)
			response.Params.Period, response.Params.PeriodKey = tc.period, tc.key
			response.Blocks = []*commonv1.Block{{Id: "summary", Type: "text", V: 1, Data: coverageAPIData(t, `{"text":"Данные за период"}`), Action: &commonv1.BlockAction{Type: "open_operations", Filter: &commonv1.OperationsFilter{From: generationAPIRange().Start, To: generationAPIRange().End, Kind: commonv1.KindFilter_KIND_FILTER_INCOME, TransferMode: commonv1.TransferMode_TRANSFER_MODE_WITHOUT}}}}
			saved := coverageAPIPresentation(t, response, tc.read, "blocks[0]", false)
			if len(saved.Chips) != 2 || saved.Chips[0].GetLabel() != "Операции по выборке" || saved.Chips[0].GetFilters().GetKind() != commonv1.KindFilter_KIND_FILTER_INCOME || saved.Chips[1].GetParams().GetPeriodKey() != tc.key || saved.Chips[1].GetParams().GetPeriod() != tc.period {
				t.Fatalf("native action or calendar params changed: %v", saved.Chips)
			}
		})
	}
}

func TestAPICoverageCatalogMissingDataAndMalformedChartsAreNeverPublished(t *testing.T) {
	for _, tc := range []struct {
		name, kind, raw, alt string
	}{
		{"unknown-without-fallback", "future", `{}`, ""},
		{"text-missing", "text", `{}`, ""},
		{"text-wrong-type", "text", `{"text":12}`, ""},
		{"rows-no-label", "rows", `{"rows":[{"value":2,"value_kind":"count"}]}`, ""},
		{"rows-no-value-or-text", "rows", `{"rows":[{"label":"Нет значения"}]}`, ""},
		{"rows-unknown-kind-without-text", "rows", `{"rows":[{"label":"Новое значение","value":null,"value_kind":"future"}]}`, ""},
		{"tile-list-wrong-type", "tile_group", `{"tiles":"private upstream text"}`, ""},
		{"tile-not-object", "tile_group", `{"tiles":["private upstream text"]}`, ""},
		{"tile-without-id", "tile_group", `{"tiles":[{"title":"Показатель"}]}`, ""},
		{"tile-status-wrong-type", "tile_group", `{"tiles":[{"id":"metric","status":17}]}`, ""},
		{"tile-future-status", "tile_group", `{"tiles":[{"id":"metric","status":"future"}]}`, ""},
		{"locked-tile-without-lock", "tile_group", `{"tiles":[{"id":"metric","status":"locked"}]}`, ""},
		{"locked-tile-invalid-lock", "tile_group", `{"tiles":[{"id":"metric","status":"locked","lock":"private upstream text"}]}`, ""},
		{"value-kind-wrong-type", "rows", `{"rows":[{"label":"Расходы","value_kind":17,"value":123}]}`, ""},
		{"amount-beyond-exact-integer", "rows", `{"rows":[{"label":"Расходы","value_kind":"amount","value":9007199254740992}]}`, ""},
		{"chart-missing-fallback", "chart", `{"kind":"line","points":[]}`, ""},
		{"chart-bad-highlight", "chart", `{"kind":"bar","value_kind":"percent","highlight_index":4,"points":[{"value":0.5,"coverage":"complete","range":{"from":"2026-09-01T00:00:00Z","to":"2026-10-01T00:00:00Z"}}]}`, "Доля расходов"},
		{"chart-reversed-range", "chart", `{"kind":"bar","value_kind":"percent","points":[{"value":0.5,"coverage":"complete","range":{"from":"2026-10-01T00:00:00Z","to":"2026-09-01T00:00:00Z"}}]}`, "Доля расходов"},
		{"chart-no-data-with-value", "chart", `{"kind":"bar","value_kind":"percent","points":[{"value":0.5,"coverage":"no_data","range":{"from":"2026-09-01T00:00:00Z","to":"2026-10-01T00:00:00Z"}}]}`, "Доля расходов"},
		{"chart-data-outside-range", "chart", `{"kind":"bar","value_kind":"percent","points":[{"value":0.5,"coverage":"partial","data_from":"2026-08-01T00:00:00Z","range":{"from":"2026-09-01T00:00:00Z","to":"2026-10-01T00:00:00Z"}}]}`, "Доля расходов"},
		{"chart-reversed-bounds", "chart", `{"kind":"bar","value_kind":"percent","points":[{"value":0.5,"coverage":"partial","data_from":"2026-09-20T00:00:00Z","data_to":"2026-09-05T00:00:00Z","range":{"from":"2026-09-01T00:00:00Z","to":"2026-10-01T00:00:00Z"}}]}`, "Доля расходов"},
		{"chart-percent-outside-unit", "chart", `{"kind":"bar","value_kind":"percent","points":[{"value":1.5,"coverage":"complete","range":{"from":"2026-09-01T00:00:00Z","to":"2026-10-01T00:00:00Z"}}]}`, "Доля расходов"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := presenterAnalytics(t)
			block := &commonv1.Block{Id: "malformed", Type: tc.kind, V: 1, Data: coverageAPIData(t, tc.raw)}
			if tc.alt != "" {
				block.AltText = &tc.alt
			}
			response.Blocks = []*commonv1.Block{block}
			coverageAPIPresentation(t, response, `{"period":"month","period_key":"2026-09"}`, "blocks[0]", true)
		})
	}
}

func TestAPICoverageCatalogAncestorStatesAndLockFallbackDoNotExposeHiddenValues(t *testing.T) {
	for _, tc := range []struct {
		name, parentStatus, empty, raw, want string
		charged                              bool
	}{
		{"ancestor-none", "none", "", `{"tiles":[{"id":"metric","title":"Прогноз","value_kind":"amount","value":12345}]}`, "Показатель пока недоступен", true},
		{"ancestor-unavailable", "unavailable", "Нет полного периода", `{"tiles":[{"id":"metric","title":"Прогноз","value_kind":"amount","value":12345}]}`, "Нет полного периода", true},
		{"locked-without-optional-text", "", "", `{"tiles":[{"id":"metric","title":"Прогноз","status":"locked","value_kind":"amount","value":12345,"lock":{}}]}`, "Пока недоступно", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := presenterAnalytics(t)
			block := &commonv1.Block{Id: "forecast", Type: "tile_group", V: 1, Status: tc.parentStatus, Data: coverageAPIData(t, tc.raw)}
			if tc.empty != "" {
				block.EmptyText = &tc.empty
			}
			response.Blocks = []*commonv1.Block{block}
			saved := coverageAPIPresentation(t, response, `{"period":"month","period_key":"2026-09"}`, "blocks[0].data.tiles[0]", false, tc.charged)
			text := saved.Blocks[0].GetText().GetText()
			if !strings.Contains(text, tc.want) || strings.Contains(text, "123") || saved.Blocks[0].GetRows() != nil {
				t.Fatalf("hidden value leaked or trusted state text missing: %v", saved.Blocks[0])
			}
		})
	}
}

func TestAPICoverageCatalogPercentNoDataAndPointNotesRetainMeaning(t *testing.T) {
	for _, tc := range []struct {
		name, raw, alt string
		chart          bool
	}{
		{"percent-and-no-data", `{"kind":"bar","value_kind":"percent","highlight_index":0,"points":[{"value":0.25,"coverage":"partial","data_from":"2026-09-05T00:00:00Z","data_to":"2026-09-20T00:00:00Z","range":{"from":"2026-09-01T00:00:00Z","to":"2026-10-01T00:00:00Z"}},{"coverage":"no_data","range":{"from":"2026-10-01T00:00:00Z","to":"2026-11-01T00:00:00Z"}}]}`, "Сентябрь: 25%, октябрь: нет данных", true},
		{"point-note-fallback", `{"kind":"bar","value_kind":"percent","points":[{"value":0.25,"note":"Неполный период","coverage":"partial","range":{"from":"2026-09-01T00:00:00Z","to":"2026-10-01T00:00:00Z"}}]}`, "25% за неполный период", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := presenterAnalytics(t)
			response.Blocks = []*commonv1.Block{{Id: "percent", Type: "chart", V: 1, AltText: &tc.alt, Data: coverageAPIData(t, tc.raw)}}
			saved := coverageAPIPresentation(t, response, `{"period":"month","period_key":"2026-09"}`, "blocks[0]", false)
			block := saved.Blocks[0]
			if tc.chart {
				chart := block.GetChart()
				if chart == nil || chart.GetValueKind() != assistantv1.ValueKind_VALUE_KIND_PERCENT || len(chart.Points) != 2 || chart.Points[0].GetValue() != 0.25 || chart.Points[1].GetBounds().GetCoverage() != commonv1.Coverage_COVERAGE_NO_DATA || chart.Points[1].Value != nil {
					t.Fatalf("percent/absent value was fabricated: %v", chart)
				}
			} else if !strings.Contains(block.GetText().GetText(), tc.alt) {
				t.Fatalf("point note lost in fallback: %v", block)
			}
		})
	}
}
