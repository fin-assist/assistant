package app_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	assistantv1 "github.com/fin-assist/contracts/gen/go/finassist/assistant/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	statementv1 "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"

	"github.com/fin-assist/assistant/internal/app/apitest"
)

const presenterCategory = "20000000-0000-4000-8000-000000000001"
const presenterExact int64 = 9007199254740001

func presenterAggregate() *statementv1.AggregateOperationsResponse {
	category := presenterCategory
	return &statementv1.AggregateOperationsResponse{TotalAmountMinor: presenterExact, TotalOperationCount: 1, Range: generationAPIRange(), Bounds: generationAPIBounds(), TimeZone: "Europe/Moscow", CalculatedAt: timestamppb.New(generationAPINow), Version: &statementv1.DataVersion{Revisions: strictAPIWire(1, 1, 1)}, Groups: []*statementv1.AggregateGroup{{Keys: []*statementv1.GroupKey{{Category: &statementv1.CategoryRef{Id: category, Name: "Еда"}}}, AmountMinor: presenterExact, OperationCount: 1, AverageMinor: presenterExact, Filters: &commonv1.OperationsFilter{CategoryId: &category, From: generationAPIRange().Start, To: generationAPIRange().End, TransferMode: commonv1.TransferMode_TRANSFER_MODE_WITHOUT}}}}
}
func presenterHistory(t *testing.T, env *apitest.Env, user string) *assistantv1.Message {
	t.Helper()
	page, err := assistantv1.NewAssistantServiceClient(env.Conn).ListMessages(generationAPIContext(t, user, ""), &assistantv1.ListMessagesRequest{})
	if err != nil || len(page.GetMessages()) < 2 {
		t.Fatalf("presented history: %v %v", page, err)
	}
	return page.Messages[len(page.Messages)-1]
}
func presenterSnapshot(t *testing.T, env *apitest.Env, user, id string) *assistantv1.AnswerSnapshot {
	t.Helper()
	ctx, cancel := context.WithCancel(generationAPIContext(t, user, ""))
	defer cancel()
	stream, err := assistantv1.NewAssistantServiceClient(env.Conn).WatchAnswer(ctx, &assistantv1.WatchAnswerRequest{AnswerId: id})
	if err != nil {
		t.Fatal(err)
	}
	first, err := stream.Recv()
	if err != nil || first.GetSnapshot() == nil {
		t.Fatalf("presented snapshot: %v %v", first, err)
	}
	return first.GetSnapshot()
}
func presenterWait(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("presenter continuation did not reach barrier")
	}
}
func presenterAssertFailure(t *testing.T, env *apitest.Env, user, answer string) {
	t.Helper()
	done, failure := generationAPITerminal(t, env, user, answer)
	if done != nil || failure.GetCode() != assistantv1.AnswerErrorCode_ANSWER_ERROR_CODE_MODEL_ERROR || !failure.GetRetryable() || failure.GetRemainingLimit() != 5 {
		t.Fatalf("presentation failure: done=%v error=%v", done, failure)
	}
	saved := presenterHistory(t, env, user)
	if saved.GetStatus() != assistantv1.AnswerStatus_ANSWER_STATUS_FAILED || len(saved.GetBlocks()) != 0 || saved.GetSource() != nil || len(saved.GetChips()) != 0 {
		t.Fatalf("failed presentation leaked partial content: %v", saved)
	}
	var partial int
	var sourceCleared, chipsCleared, charged bool
	err := env.App.DB.Writer(t.Context()).QueryRow(t.Context(), `SELECT
 (SELECT count(*) FROM answer_block b WHERE b.user_id=a.user_id AND b.answer_id=a.id),
 calculated_at IS NULL AND data_from IS NULL AND data_to IS NULL AND data_revisions IS NULL,chips='[]'::jsonb,charged
 FROM answer a WHERE user_id=$1 AND id=$2`, user, answer).Scan(&partial, &sourceCleared, &chipsCleared, &charged)
	if err != nil || partial != 0 || !sourceCleared || !chipsCleared || charged {
		t.Fatalf("failed presentation retained durable partial data: blocks=%d sourceClear=%v chipsClear=%v charged=%v %v", partial, sourceCleared, chipsCleared, charged, err)
	}
}

func TestAPIGenerationPresenterAPIRowsAreDurableBeforeFinalText(t *testing.T) {
	p := &generationAPIPeers{grant: true, aggregateResponse: presenterAggregate()}
	entered, release := make(chan struct{}), make(chan struct{})
	presentEntered, presentRelease := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	var presentOnce sync.Once
	unblockPresent := func() { presentOnce.Do(func() { close(presentRelease) }) }
	defer unblockPresent()
	m := generationAPIModel(t, func(n int, w http.ResponseWriter, r *http.Request) {
		switch n {
		case 1:
			_, _ = io.WriteString(w, generationAPIFunctionID("aggregate", "aggregate_operations", `{"all_time":true,"group_by":["category"]}`))
		case 2:
			close(presentEntered)
			select {
			case <-r.Context().Done():
				return
			case <-presentRelease:
			}
			_, _ = io.WriteString(w, generationAPIFunctionID("presentation", "present_result", `{"result_id":"aggregate","view":"rows"}`))
		default:
			close(entered)
			select {
			case <-r.Context().Done():
				return
			case <-release:
			}
			_, _ = io.WriteString(w, generationAPIText("Итог по категориям"))
		}
	})
	env := generationAPIEnv(t, p, m)
	accepted := generationAPIAsk(t, env, generationAPIUser, generationAPIKey(), "Расходы по категориям", commonv1.TransferMode_TRANSFER_MODE_WITHOUT)
	presenterWait(t, presentEntered)
	live, err := assistantv1.NewAssistantServiceClient(env.Conn).WatchAnswer(generationAPIContext(t, generationAPIUser, ""), &assistantv1.WatchAnswerRequest{AnswerId: accepted.AnswerId})
	if err != nil {
		t.Fatal(err)
	}
	initial, err := live.Recv()
	if err != nil || initial.GetSnapshot().GetStatus() != assistantv1.AnswerStatus_ANSWER_STATUS_GENERATING || len(initial.GetSnapshot().GetBlocks()) != 0 {
		t.Fatalf("before presentation snapshot: %v %v", initial, err)
	}
	unblockPresent()
	presenterWait(t, entered)
	blockEvent, err := live.Recv()
	if err != nil || blockEvent.GetBlock() == nil || blockEvent.GetBlock().GetIndex() != 0 || blockEvent.GetBlock().GetBlock().GetRows() == nil {
		t.Fatalf("ready rows stream event: %v %v", blockEvent, err)
	}
	snapshot := presenterSnapshot(t, env, generationAPIUser, accepted.AnswerId)
	if snapshot.GetStatus() != assistantv1.AnswerStatus_ANSWER_STATUS_GENERATING || len(snapshot.Blocks) != 1 || snapshot.Blocks[0].GetRows() == nil || len(snapshot.Blocks[0].GetRows().GetRows()) != 5 || snapshot.Blocks[0].GetRows().Rows[2].GetLabel() != "Еда" || snapshot.Blocks[0].GetRows().Rows[2].GetAmountMinor() != presenterExact || snapshot.Blocks[0].GetRows().Rows[0].GetAmountMinor() != presenterExact || snapshot.Blocks[0].GetAltText() == "" {
		t.Fatalf("ready rows before terminal: %v", snapshot)
	}
	// A second subscription starts from the same ready block while the HTTP model
	// continuation is still blocked; no client depends on the original subscription.
	reconnect := presenterSnapshot(t, env, generationAPIUser, accepted.AnswerId)
	if !proto.Equal(snapshot, reconnect) {
		t.Fatalf("reconnect changed durable block: %v %v", snapshot, reconnect)
	}
	unblock()
	done, failed := generationAPITerminal(t, env, generationAPIUser, accepted.AnswerId)
	if failed != nil || done == nil || !done.Charged || done.RemainingLimit != 4 {
		t.Fatalf("rows terminal: %v %v", done, failed)
	}
	answer := presenterHistory(t, env, generationAPIUser)
	if len(answer.GetBlocks()) != 2 || !proto.Equal(answer.Blocks[0], snapshot.Blocks[0]) || answer.Blocks[1].GetText().GetText() != "Итог по категориям" || answer.GetSource().GetTransferMode() != commonv1.TransferMode_TRANSFER_MODE_WITHOUT || len(answer.GetChips()) != 1 || answer.Chips[0].GetFilters().GetCategoryId() != presenterCategory || answer.Chips[0].GetFilters().GetTransferMode() != commonv1.TransferMode_TRANSFER_MODE_WITHOUT || p.count("aggregate") != 1 || m.count.Load() != 3 {
		t.Fatalf("saved presentation changed scope, values or positions: %v", answer)
	}
}

func presenterBatch(calls ...[3]string) string {
	events := []map[string]any{generationAPICreated()}
	var output []any
	for i, call := range calls {
		id, name, args := call[0], call[1], call[2]
		item := map[string]any{"id": id, "type": "function_call", "call_id": id, "name": name, "arguments": args, "status": "completed"}
		events = append(events, map[string]any{"type": "response.output_item.added", "output_index": i, "item": map[string]any{"id": id, "type": "function_call", "call_id": id, "name": name, "arguments": "", "status": "in_progress"}}, map[string]any{"type": "response.function_call_arguments.delta", "output_index": i, "item_id": id, "delta": args}, map[string]any{"type": "response.function_call_arguments.done", "output_index": i, "item_id": id, "name": name, "arguments": args}, map[string]any{"type": "response.output_item.done", "output_index": i, "item": item})
		output = append(output, item)
	}
	events = append(events, map[string]any{"type": "response.completed", "response": map[string]any{"id": "r", "status": "completed", "output": output, "usage": map[string]any{"input_tokens": 10, "output_tokens": 4, "total_tokens": 14, "tool_tokens": 0}}})
	return generationAPIFrames(events...)
}
func TestAPIGenerationPresenterAPIRejectsUntrustedReferencesAndArguments(t *testing.T) {
	for _, tc := range []struct {
		name, args string
		sameBatch  bool
	}{
		{name: "unseen", args: `{"result_id":"unknown","view":"rows"}`},
		{name: "numbers", args: `{"result_id":"aggregate","view":"rows","values":[999999]}`},
		{name: "filters", args: `{"result_id":"aggregate","view":"rows","filters":{"user_id":"foreign"}}`},
		{name: "unknown-path", args: `{"result_id":"aggregate","view":"rows","path":"blocks[999]"}`},
		{name: "same-batch", args: `{"result_id":"same","view":"rows"}`, sameBatch: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &generationAPIPeers{grant: true, aggregateResponse: presenterAggregate()}
			m := generationAPIModel(t, func(n int, w http.ResponseWriter, _ *http.Request) {
				if tc.sameBatch {
					_, _ = io.WriteString(w, presenterBatch([3]string{"same", "aggregate_operations", `{"all_time":true,"group_by":["category"]}`}, [3]string{"present", "present_result", tc.args}))
					return
				}
				if n == 1 {
					_, _ = io.WriteString(w, generationAPIFunctionID("aggregate", "aggregate_operations", `{"all_time":true,"group_by":["category"]}`))
					return
				}
				_, _ = io.WriteString(w, generationAPIFunctionID("present", "present_result", tc.args))
			})
			env := generationAPIEnv(t, p, m)
			accepted := generationAPIAsk(t, env, generationAPIUser, generationAPIKey(), "Финансовый вопрос", commonv1.TransferMode_TRANSFER_MODE_WITHOUT)
			presenterAssertFailure(t, env, generationAPIUser, accepted.AnswerId)
			expected := 1
			if tc.sameBatch {
				expected = 0
			}
			if p.count("aggregate") != expected || p.count("find") != 0 || p.count("analytics") != 0 {
				t.Fatal("invalid presentation executed extra data RPC")
			}
			expectedSteps := int32(2)
			if tc.sameBatch {
				expectedSteps = 1
			}
			if m.count.Load() != expectedSteps {
				t.Fatalf("invalid presentation reached a subsequent model step: %d", m.count.Load())
			}
		})
	}
}
func TestAPIGenerationPresenterAPIReferenceFromAnotherAnswerIsNotVisible(t *testing.T) {
	p := &generationAPIPeers{grant: true, aggregateResponse: presenterAggregate()}
	m := generationAPIModel(t, func(n int, w http.ResponseWriter, _ *http.Request) {
		switch n {
		case 1:
			_, _ = io.WriteString(w, generationAPIFunctionID("foreign-result", "aggregate_operations", `{"all_time":true,"group_by":["category"]}`))
		case 2:
			_, _ = io.WriteString(w, generationAPIText("Готово"))
		default:
			_, _ = io.WriteString(w, generationAPIFunctionID("present", "present_result", `{"result_id":"foreign-result","view":"rows"}`))
		}
	})
	env := generationAPIEnv(t, p, m)
	other := generationAPIAsk(t, env, generationAPIOther, generationAPIKey(), "Другой вопрос", commonv1.TransferMode_TRANSFER_MODE_WITHOUT)
	generationAPITerminal(t, env, generationAPIOther, other.AnswerId)
	own := generationAPIAsk(t, env, generationAPIUser, generationAPIKey(), "Мой вопрос", commonv1.TransferMode_TRANSFER_MODE_WITHOUT)
	presenterAssertFailure(t, env, generationAPIUser, own.AnswerId)
	if p.count("aggregate") != 1 || m.count.Load() != 3 {
		t.Fatal("foreign result lookup triggered a new data read")
	}
}
func TestAPIGenerationPresenterAPIFailureAfterReadyRowsClearsAllPartial(t *testing.T) {
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
		default:
			close(entered)
			select {
			case <-r.Context().Done():
				return
			case <-release:
			}
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	env := generationAPIEnv(t, p, m)
	accepted := generationAPIAsk(t, env, generationAPIUser, generationAPIKey(), "Расходы", commonv1.TransferMode_TRANSFER_MODE_WITHOUT)
	presenterWait(t, entered)
	if snapshot := presenterSnapshot(t, env, generationAPIUser, accepted.AnswerId); len(snapshot.Blocks) != 1 || snapshot.Blocks[0].GetRows() == nil {
		t.Fatalf("rows were not persisted before failure: %v", snapshot)
	}
	unblock()
	presenterAssertFailure(t, env, generationAPIUser, accepted.AnswerId)
}

func presenterAnalytics(t *testing.T) *analyticsv1.GetAnalyticsResponse {
	t.Helper()
	data, err := structpb.NewStruct(map[string]any{"tiles": []any{map[string]any{"id": "expenses", "label": "Расходы", "title": "Расходы", "value_kind": "amount", "value": 12345, "status": "ready"}, map[string]any{"id": "forecast", "label": "Прогноз", "title": "Прогноз", "value_kind": "amount", "status": "locked", "lock": map[string]any{"reason": "need_full_months", "text": "Нужно 3 полных месяца"}}}})
	if err != nil {
		t.Fatal(err)
	}
	alt := "Расходы и прогноз"
	return &analyticsv1.GetAnalyticsResponse{HasData: true, Params: &commonv1.AnalyticsParams{Period: commonv1.PeriodType_PERIOD_TYPE_MONTH, PeriodKey: "2026-09", TransferMode: commonv1.TransferMode_TRANSFER_MODE_WITHOUT}, Period: &analyticsv1.PeriodInfo{Range: generationAPIRange(), Bounds: generationAPIBounds()}, State: &analyticsv1.ScreenState{DataFrom: generationAPIRange().Start, DataTo: generationAPIRange().End, CalculatedAt: timestamppb.New(generationAPINow), FullMonths: 1}, TimeZone: "Europe/Moscow", Currency: "RUB", Blocks: []*commonv1.Block{{Id: "tiles", Type: "tile_group", V: 1, AltText: &alt, Data: data}}}
}
func TestAPIGenerationPresenterAPISelectedAnalyticsLockControlsRefund(t *testing.T) {
	for _, selected := range []struct {
		name, path string
		charged    bool
	}{{"ready", "blocks[0].data.tiles[0]", true}, {"locked", "blocks[0].data.tiles[1]", false}, {"ancestor-locked", "blocks[0].data.tiles[0]", false}} {
		t.Run(selected.name, func(t *testing.T) {
			p := &generationAPIPeers{grant: true, analyticsResponse: presenterAnalytics(t)}
			if selected.name == "ancestor-locked" {
				p.analyticsResponse.Blocks[0].Status = "locked"
				p.analyticsResponse.Blocks[0].Lock = &commonv1.BlockLock{Reason: "need_full_months", Text: "Нужно 3 полных месяца"}
			}
			args, _ := json.Marshal(map[string]any{"result_id": "analytics", "view": "analytics", "path": selected.path})
			m := generationAPIModel(t, func(n int, w http.ResponseWriter, _ *http.Request) {
				switch n {
				case 1:
					_, _ = io.WriteString(w, generationAPIFunctionID("analytics", "get_analytics", `{"period":"month","period_key":"2026-09"}`))
				case 2:
					_, _ = io.WriteString(w, generationAPIFunctionID("present", "present_result", string(args)))
				default:
					_, _ = io.WriteString(w, generationAPIText("Аналитика показана"))
				}
			})
			env := generationAPIEnv(t, p, m)
			accepted := generationAPIAsk(t, env, generationAPIUser, generationAPIKey(), "Покажи аналитику", commonv1.TransferMode_TRANSFER_MODE_WITHOUT)
			done, failed := generationAPITerminal(t, env, generationAPIUser, accepted.AnswerId)
			remaining := int32(5)
			if selected.charged {
				remaining = 4
			}
			if failed != nil || done == nil || done.Charged != selected.charged || done.RemainingLimit != remaining {
				t.Fatalf("selected analytics charge: %v %v", done, failed)
			}
			saved := presenterHistory(t, env, generationAPIUser)
			if saved.GetSource() == nil || saved.GetSource().GetTransferMode() != commonv1.TransferMode_TRANSFER_MODE_WITHOUT || len(saved.GetChips()) != 1 || saved.Chips[0].GetParams().GetTransferMode() != commonv1.TransferMode_TRANSFER_MODE_WITHOUT || saved.Chips[0].GetParams().GetPeriodKey() != "2026-09" {
				t.Fatalf("analytics source/chips: %v", saved)
			}
			if selected.charged {
				if len(saved.Blocks) != 2 || saved.Blocks[0].GetRows() == nil || saved.Blocks[0].GetRows().Rows[0].GetAmountMinor() != 12345 {
					t.Fatalf("ready metric: %v", saved)
				}
			} else if len(saved.Blocks) < 2 || !strings.Contains(saved.Blocks[0].GetText().GetText(), "Нужно 3 полных месяца") {
				t.Fatalf("trusted lock text not shown: %v", saved)
			}
		})
	}
}

func presenterMonthlyPeers() *generationAPIPeers {
	zone := time.FixedZone("Moscow", 3*60*60)
	august := time.Date(2026, 8, 1, 0, 0, 0, 0, zone)
	september := august.AddDate(0, 1, 0)
	october := september.AddDate(0, 1, 0)
	monthRange := func(from, to time.Time) *commonv1.TimeRange {
		return &commonv1.TimeRange{Start: timestamppb.New(from), End: timestamppb.New(to)}
	}
	bounds := func(from, to time.Time) *commonv1.DataBounds {
		return &commonv1.DataBounds{Coverage: commonv1.Coverage_COVERAGE_COMPLETE, DataFrom: timestamppb.New(from), DataTo: timestamppb.New(to.Add(-time.Second))}
	}
	sept := "2026-09"
	return &generationAPIPeers{grant: true,
		overviewResponse:  &statementv1.GetDataOverviewResponse{HasData: true, DataFrom: timestamppb.New(august), DataTo: timestamppb.New(october), FullMonths: 2, TimeZone: "Europe/Moscow", CalculatedAt: timestamppb.New(generationAPINow), Version: &statementv1.DataVersion{Revisions: strictAPIWire(1, 1, 1)}, Months: []*statementv1.MonthCoverage{{Month: "2026-08", Range: monthRange(august, september), Bounds: bounds(august, september)}, {Month: sept, Range: monthRange(september, october), Bounds: bounds(september, october)}}},
		aggregateResponse: &statementv1.AggregateOperationsResponse{TotalAmountMinor: 12345, TotalOperationCount: 1, Range: monthRange(august, october), Bounds: bounds(august, october), TimeZone: "Europe/Moscow", CalculatedAt: timestamppb.New(generationAPINow), Version: &statementv1.DataVersion{Revisions: strictAPIWire(1, 1, 1)}, Groups: []*statementv1.AggregateGroup{{Keys: []*statementv1.GroupKey{{Month: &sept}}, AmountMinor: 12345, OperationCount: 1, AverageMinor: 12345, Filters: &commonv1.OperationsFilter{From: timestamppb.New(september), To: timestamppb.New(october), TransferMode: commonv1.TransferMode_TRANSFER_MODE_WITHOUT}}}},
	}
}
func TestAPIGenerationPresenterAPIMonthlyChartUsesTrustedCalendarAndCoverage(t *testing.T) {
	for _, variant := range []string{"valid", "version-mismatch", "legacy-empty-vectors", "utc-not-calendar"} {
		t.Run(variant, func(t *testing.T) {
			p := presenterMonthlyPeers()
			if variant == "version-mismatch" {
				p.aggregateResponse.Version.Revisions = strictAPIWire(2, 1, 1)
			}
			if variant == "legacy-empty-vectors" {
				p.overviewResponse.Version.Revisions = nil
				p.aggregateResponse.Version.Revisions = nil
			}
			if variant == "utc-not-calendar" {
				month := p.overviewResponse.Months[1]
				month.Range = generationAPIRange()
				month.Bounds = generationAPIBounds()
				p.aggregateResponse.Groups[0].Filters.From = month.Range.Start
				p.aggregateResponse.Groups[0].Filters.To = month.Range.End
			}
			m := generationAPIModel(t, func(n int, w http.ResponseWriter, _ *http.Request) {
				switch n {
				case 1:
					_, _ = io.WriteString(w, generationAPIFunctionID("coverage", "get_data_overview", `{}`))
				case 2:
					_, _ = io.WriteString(w, generationAPIFunctionID("aggregate", "aggregate_operations", `{"all_time":true,"group_by":["month"]}`))
				case 3:
					_, _ = io.WriteString(w, generationAPIFunctionID("present", "present_result", `{"result_id":"aggregate","view":"chart","coverage_result_id":"coverage"}`))
				default:
					_, _ = io.WriteString(w, generationAPIText("Расходы по месяцам"))
				}
			})
			env := generationAPIEnv(t, p, m)
			accepted := generationAPIAsk(t, env, generationAPIUser, generationAPIKey(), "Покажи месяцы", commonv1.TransferMode_TRANSFER_MODE_WITHOUT)
			if variant != "valid" {
				presenterAssertFailure(t, env, generationAPIUser, accepted.AnswerId)
				return
			}
			done, failed := generationAPITerminal(t, env, generationAPIUser, accepted.AnswerId)
			if done == nil || !done.Charged || failed != nil {
				t.Fatalf("chart terminal: %v %v", done, failed)
			}
			answer := presenterHistory(t, env, generationAPIUser)
			if len(answer.Blocks) != 2 || answer.Blocks[0].GetChart() == nil || len(answer.Blocks[0].GetChart().GetPoints()) != 1 || answer.Blocks[0].GetAltText() == "" {
				t.Fatalf("chart fabricated absent August group or omitted alt text: %v", answer)
			}
			point := answer.Blocks[0].GetChart().Points[0]
			month := p.overviewResponse.Months[1]
			if point.GetValue() != 12345 || !proto.Equal(point.GetRange(), month.Range) || point.GetBounds().GetCoverage() != commonv1.Coverage_COVERAGE_COMPLETE || len(answer.Chips) != 1 || !proto.Equal(answer.Chips[0].GetFilters().GetFrom(), month.Range.Start) || !proto.Equal(answer.Chips[0].GetFilters().GetTo(), month.Range.End) || answer.Chips[0].GetFilters().GetTransferMode() != commonv1.TransferMode_TRANSFER_MODE_WITHOUT {
				t.Fatalf("chart calendar/value/chip changed: %v", answer)
			}
		})
	}
}

func presenterCitedText(text string, urls ...string) string {
	search := map[string]any{"id": "search", "type": "web_search_call", "status": "completed"}
	var annotations []any
	for _, url := range urls {
		annotations = append(annotations, map[string]any{"type": "url_citation", "url": url, "title": "", "start_index": 0, "end_index": 0})
	}
	part := map[string]any{"type": "output_text", "text": text, "annotations": annotations}
	message := map[string]any{"id": "message", "type": "message", "role": "assistant", "status": "completed", "content": []any{part}}
	return generationAPIFrames(generationAPICreated(),
		map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"id": "search", "type": "web_search_call", "status": "in_progress"}},
		map[string]any{"type": "response.output_item.done", "output_index": 0, "item": search},
		map[string]any{"type": "response.output_item.added", "output_index": 1, "item": map[string]any{"id": "message", "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}}},
		map[string]any{"type": "response.content_part.added", "output_index": 1, "item_id": "message", "content_index": 0, "part": map[string]any{"type": "output_text", "text": ""}},
		map[string]any{"type": "response.output_text.delta", "output_index": 1, "item_id": "message", "content_index": 0, "delta": text},
		map[string]any{"type": "response.output_text.done", "output_index": 1, "item_id": "message", "content_index": 0, "text": text},
		map[string]any{"type": "response.content_part.done", "output_index": 1, "item_id": "message", "content_index": 0, "part": part},
		map[string]any{"type": "response.output_item.done", "output_index": 1, "item": message},
		map[string]any{"type": "response.completed", "response": map[string]any{"id": "r", "status": "completed", "output": []any{search, message}, "usage": map[string]any{"input_tokens": 10, "output_tokens": 4, "total_tokens": 14, "tool_tokens": 0}}})
}
func TestAPIGenerationPresenterAPICitationsUseOnlyProviderAnnotations(t *testing.T) {
	for _, unsafe := range []bool{false, true} {
		t.Run(map[bool]string{false: "official-bare-host-empty-title-zero-range", true: "unsafe-url"}[unsafe], func(t *testing.T) {
			p := &generationAPIPeers{grant: true}
			url := "www.rbc.ru"
			if unsafe {
				url = "javascript:alert(1)"
			}
			m := generationAPIModel(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, presenterCitedText("Справка о ставках. https://fabricated.invalid/model-link", url, url))
			})
			env := generationAPIEnv(t, p, m)
			accepted := generationAPIAsk(t, env, generationAPIUser, generationAPIKey(), "Общие финансовые сведения", commonv1.TransferMode_TRANSFER_MODE_WITHOUT)
			if unsafe {
				presenterAssertFailure(t, env, generationAPIUser, accepted.AnswerId)
				return
			}
			done, failed := generationAPITerminal(t, env, generationAPIUser, accepted.AnswerId)
			if done == nil || !done.Charged || failed != nil {
				t.Fatalf("safe web references terminal: %v %v", done, failed)
			}
			answer := presenterHistory(t, env, generationAPIUser)
			if len(answer.Blocks) != 2 {
				t.Fatalf("sources block missing: %v", answer)
			}
			sources := answer.Blocks[1].GetText().GetText()
			if strings.Count(sources, "https://www.rbc.ru") != 1 || strings.Contains(sources, "fabricated.invalid") || len(answer.Chips) != 0 || answer.Source != nil {
				t.Fatalf("deduplicated provider sources: %q", sources)
			}
		})
	}
}

func TestAPIGenerationPresenterAPIFindRowsPreservePageCurrencyAndOriginalFilters(t *testing.T) {
	rangeValue := generationAPIRange()
	query, category, selection := "Кофе", presenterCategory, "s_merchant_coffee"
	amountFrom, amountTo := int64(100), int64(9007199254740991)
	expectedFilter := &commonv1.OperationsFilter{
		From: rangeValue.Start, To: rangeValue.End, Q: &query, CategoryId: &category,
		Selection: &selection, Kind: commonv1.KindFilter_KIND_FILTER_EXPENSE,
		AmountFromMinor: &amountFrom, AmountToMinor: &amountTo, TransferMode: commonv1.TransferMode_TRANSFER_MODE_WITHOUT,
	}
	p := &generationAPIPeers{grant: true, findResponse: &statementv1.FindOperationsResponse{
		TotalCount: 7, Range: rangeValue, Bounds: generationAPIBounds(), TimeZone: "Europe/Moscow", Version: &statementv1.DataVersion{},
		Items: []*statementv1.Operation{
			{Id: "30000000-0000-4000-8000-000000000011", Kind: statementv1.OperationKind_OPERATION_KIND_EXPENSE, Status: statementv1.OperationStatus_OPERATION_STATUS_POSTED, Title: "Кофе в рублях", AmountMinor: presenterExact, Currency: "RUB", OccurredAt: timestamppb.New(rangeValue.Start.AsTime().Add(24 * time.Hour))},
			{Id: "30000000-0000-4000-8000-000000000012", Kind: statementv1.OperationKind_OPERATION_KIND_EXPENSE, Status: statementv1.OperationStatus_OPERATION_STATUS_POSTED, Title: "Кофе за границей", AmountMinor: 12345, Currency: "USD", OccurredAt: timestamppb.New(rangeValue.Start.AsTime().Add(48 * time.Hour))},
			{Id: "30000000-0000-4000-8000-000000000013", Kind: statementv1.OperationKind_OPERATION_KIND_EXPENSE, Status: statementv1.OperationStatus_OPERATION_STATUS_POSTED, Title: "Кофе в Японии", AmountMinor: 12345, Currency: "JPY", OccurredAt: timestamppb.New(rangeValue.Start.AsTime().Add(72 * time.Hour))},
			{Id: "30000000-0000-4000-8000-000000000014", Kind: statementv1.OperationKind_OPERATION_KIND_EXPENSE, Status: statementv1.OperationStatus_OPERATION_STATUS_POSTED, Title: "Кофе в Кувейте", AmountMinor: 12345, Currency: "KWD", OccurredAt: timestamppb.New(rangeValue.Start.AsTime().Add(96 * time.Hour))},
			{Id: "30000000-0000-4000-8000-000000000015", Kind: statementv1.OperationKind_OPERATION_KIND_EXPENSE, Status: statementv1.OperationStatus_OPERATION_STATUS_POSTED, Title: "Будущая валюта", AmountMinor: 12345, Currency: "ZZZ", OccurredAt: timestamppb.New(rangeValue.Start.AsTime().Add(120 * time.Hour))},
		},
	}}
	args, err := json.Marshal(map[string]any{"filter": map[string]any{
		"from": rangeValue.Start.AsTime(), "to": rangeValue.End.AsTime(), "q": query, "category_id": category, "selection": selection, "kind": "expense", "amount_from_minor": amountFrom, "amount_to_minor": amountTo,
	}, "order": "largest", "limit": 5})
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	m := generationAPIModel(t, func(n int, w http.ResponseWriter, r *http.Request) {
		switch n {
		case 1:
			_, _ = io.WriteString(w, generationAPIFunctionID("operations-page", "find_operations", string(args)))
		case 2:
			_, _ = io.WriteString(w, generationAPIFunctionID("present-page", "present_result", `{"result_id":"operations-page","view":"rows"}`))
		default:
			close(entered)
			select {
			case <-r.Context().Done():
				return
			case <-release:
			}
			_, _ = io.WriteString(w, generationAPIText("Показана часть найденных операций"))
		}
	})
	env := generationAPIEnv(t, p, m)
	accepted := generationAPIAsk(t, env, generationAPIUser, generationAPIKey(), "Покажи найденные операции", commonv1.TransferMode_TRANSFER_MODE_WITHOUT)
	presenterWait(t, entered)
	snapshot := presenterSnapshot(t, env, generationAPIUser, accepted.AnswerId)
	if snapshot.GetStatus() != assistantv1.AnswerStatus_ANSWER_STATUS_GENERATING || len(snapshot.Blocks) != 1 || snapshot.Blocks[0].GetRows() == nil {
		t.Fatalf("ready operation rows missing before terminal: %v", snapshot)
	}
	rows := snapshot.Blocks[0].GetRows().GetRows()
	if len(rows) != 7 || rows[0].GetLabel() != "Всего совпадений" || rows[0].GetCount() != 7 || rows[1].GetAmountMinor() != presenterExact || rows[1].GetLabel() != "Расход: Кофе в рублях" || rows[1].GetSublabel() != "02.09.2026" {
		t.Fatalf("page count, exact RUB or trusted title changed: %v", rows)
	}
	if rows[2].GetLabel() != "Расход: Кофе за границей" || rows[2].GetText() != "123,45 USD" || rows[2].GetAmountMinor() != 0 || strings.Contains(rows[2].GetText(), "₽") || rows[2].GetSublabel() != "03.09.2026" {
		t.Fatalf("foreign amount was mixed into RUB: %v", rows[2])
	}
	for index, want := range map[int]string{3: "12345 JPY", 4: "12,345 KWD", 5: "12345 минимальных единиц ZZZ"} {
		if rows[index].GetText() != want || rows[index].GetAmountMinor() != 0 {
			t.Fatalf("ISO decimal scale was guessed or mixed into RUB at row %d: %v", index, rows[index])
		}
	}
	if rows[6].GetLabel() != "Выборка" || !strings.Contains(rows[6].GetText(), "только часть операций") || snapshot.Blocks[0].GetAltText() == "" {
		t.Fatalf("partial-page notice or alt text missing: %v", rows)
	}
	for _, row := range rows {
		if row.GetLabel() == "Сумма" || row.GetLabel() == "Итого" {
			t.Fatalf("page sum was presented as the full selection total: %v", rows)
		}
	}
	unblock()
	done, failed := generationAPITerminal(t, env, generationAPIUser, accepted.AnswerId)
	if failed != nil || done == nil || !done.Charged || done.RemainingLimit != 4 {
		t.Fatalf("find presentation terminal: %v %v", done, failed)
	}
	saved := presenterHistory(t, env, generationAPIUser)
	if len(saved.Blocks) != 2 || !proto.Equal(saved.Blocks[0], snapshot.Blocks[0]) || saved.Blocks[1].GetText().GetText() != "Показана часть найденных операций" || saved.GetSource() == nil || saved.GetSource().GetTransferMode() != commonv1.TransferMode_TRANSFER_MODE_WITHOUT || !proto.Equal(saved.GetSource().GetDataRange(), rangeValue) {
		t.Fatalf("operation rows changed in history: %v", saved)
	}
	if len(saved.Chips) != 1 || saved.Chips[0].GetScreen() != assistantv1.ChipScreen_CHIP_SCREEN_OPERATIONS || !proto.Equal(saved.Chips[0].GetFilters(), expectedFilter) {
		t.Fatalf("original operation filters were changed in chip: %v", saved.Chips)
	}
	reconnected := presenterSnapshot(t, env, generationAPIUser, accepted.AnswerId)
	if reconnected.GetStatus() != assistantv1.AnswerStatus_ANSWER_STATUS_COMPLETE || len(reconnected.Blocks) != 2 || !proto.Equal(reconnected.Blocks[0], saved.Blocks[0]) || len(reconnected.Chips) != 1 || !proto.Equal(reconnected.Chips[0], saved.Chips[0]) {
		t.Fatalf("reconnected operation snapshot: %v", reconnected)
	}
	if p.count("find") != 1 || p.count("aggregate") != 0 || p.count("overview") != 0 || p.count("analytics") != 0 || m.count.Load() != 3 {
		t.Fatal("presenting the saved page executed an additional data read")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, call := range p.calls {
		request, ok := call.request.(*statementv1.FindOperationsRequest)
		if !ok {
			continue
		}
		if request.GetUserId() != generationAPIUser || request.AllTime || request.Limit != 5 || request.Order != statementv1.ToolSortOrder_TOOL_SORT_ORDER_LARGEST || !proto.Equal(request.Filter, expectedFilter) {
			t.Fatalf("find request lost original filters or trusted scope: %v", request)
		}
		if len(call.md.Get("authorization")) != 0 || len(call.md.Get("x-service-authorization")) != 1 || call.md.Get("x-service-authorization")[0] != "Bearer dev:apitest" {
			t.Fatalf("find caller metadata leaked user identity token: %v", call.md)
		}
	}
}
