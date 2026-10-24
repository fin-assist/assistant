package app_test

import (
	"io"
	"net/http"
	"testing"

	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	statementv1 "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Downstream responses are untrusted evidence. A malformed response must never
// become a model continuation, visible answer, durable block or charged answer.
func TestAPIFinancialPeerValidationRejectsUntrustedEvidence(t *testing.T) {
	for _, name := range []string{
		"overview-zone", "overview-count", "overview-coverage-range", "overview-month-key", "overview-month-bounds",
		"aggregate-count", "aggregate-zone", "aggregate-timestamp", "aggregate-keys", "aggregate-mode", "aggregate-category", "aggregate-group-count",
		"find-count", "find-zone", "find-id", "find-currency", "find-amount", "find-timestamp", "find-kind", "find-status",
		"catalog-no-data-payload", "catalog-period", "catalog-mode", "catalog-zone", "catalog-state-count", "catalog-range",
		"catalog-block-status", "catalog-block-id", "catalog-block-version", "catalog-lock-reason", "catalog-tiles-kind", "catalog-tile-kind", "catalog-tile-id", "catalog-tile-status", "catalog-tile-lock", "catalog-integer-precision",
	} {
		t.Run(name, func(t *testing.T) {
			peers := &generationAPIPeers{grant: true}
			tool, args := "get_data_overview", `{}`
			switch name {
			case "overview-zone", "overview-count", "overview-coverage-range", "overview-month-key", "overview-month-bounds":
				peers.overviewResponse = &statementv1.GetDataOverviewResponse{HasData: true, DataFrom: generationAPIRange().Start, DataTo: generationAPIRange().End, FullMonths: 1, TimeZone: "Europe/Moscow", CalculatedAt: timestamppb.New(generationAPINow), Months: []*statementv1.MonthCoverage{{Month: "2026-09", Range: generationAPIRange(), Bounds: generationAPIBounds()}}}
				r := peers.overviewResponse
				switch name {
				case "overview-zone":
					r.TimeZone = "Local"
				case "overview-count":
					r.FullMonths = -1
				case "overview-coverage-range":
					r.DataTo = nil
				case "overview-month-key":
					r.Months[0].Month = "2026-13"
				case "overview-month-bounds":
					r.Months[0].Bounds = nil
				}
			case "aggregate-count", "aggregate-zone", "aggregate-timestamp", "aggregate-keys", "aggregate-mode", "aggregate-category", "aggregate-group-count":
				tool, args = "aggregate_operations", `{"all_time":true,"group_by":["category"]}`
				peers.aggregateResponse = presenterAggregate()
				r := peers.aggregateResponse
				switch name {
				case "aggregate-count":
					r.TotalOperationCount = -1
				case "aggregate-zone":
					r.TimeZone = "invalid"
				case "aggregate-timestamp":
					r.CalculatedAt = nil
				case "aggregate-keys":
					r.Groups[0].Keys = nil
				case "aggregate-mode":
					r.Groups[0].Filters.TransferMode = commonv1.TransferMode_TRANSFER_MODE_WITH
				case "aggregate-category":
					r.Groups[0].Keys[0].Category.Id = ""
				case "aggregate-group-count":
					r.Groups[0].OperationCount = -1
				}
			case "find-count", "find-zone", "find-id", "find-currency", "find-amount", "find-timestamp", "find-kind", "find-status":
				tool, args = "find_operations", `{"all_time":true}`
				peers.findResponse = &statementv1.FindOperationsResponse{TotalCount: 1, TimeZone: "Europe/Moscow", Bounds: generationAPIBounds(), Range: generationAPIRange(), Items: []*statementv1.Operation{{Id: "30000000-0000-4000-8000-000000000001", Kind: statementv1.OperationKind_OPERATION_KIND_EXPENSE, Status: statementv1.OperationStatus_OPERATION_STATUS_POSTED, Currency: "RUB", AmountMinor: 100, OccurredAt: timestamppb.New(generationAPINow)}}}
				r := peers.findResponse
				i := r.Items[0]
				switch name {
				case "find-count":
					r.TotalCount = 0
				case "find-zone":
					r.TimeZone = "invalid"
				case "find-id":
					i.Id = ""
				case "find-currency":
					i.Currency = "rub"
				case "find-amount":
					i.AmountMinor = -1
				case "find-timestamp":
					i.OccurredAt = nil
				case "find-kind":
					i.Kind = 0
				case "find-status":
					i.Status = 0
				}
			default:
				tool, args = "get_analytics", `{"period":"month","period_key":"2026-09"}`
				peers.analyticsResponse = presenterAnalytics(t)
				r := peers.analyticsResponse
				data, _ := structpb.NewStruct(map[string]any{"tiles": []any{map[string]any{"id": "metric", "status": "ready", "value_kind": "amount", "value": 100}}})
				b := &commonv1.Block{Id: "metric-group", Type: "tile_group", V: 1, Data: data}
				r.Blocks = []*commonv1.Block{b}
				tile := b.Data.Fields["tiles"].GetListValue().Values[0].GetStructValue()
				switch name {
				case "catalog-no-data-payload":
					r.HasData = false
				case "catalog-period":
					r.Params.PeriodKey = "2026-13"
				case "catalog-mode":
					r.Params.TransferMode = commonv1.TransferMode_TRANSFER_MODE_WITH
				case "catalog-zone":
					r.TimeZone = "invalid"
				case "catalog-state-count":
					r.State.UnreadLinesCount = -1
				case "catalog-range":
					r.State.DataTo = r.State.DataFrom
				case "catalog-block-status":
					b.Status = "future"
				case "catalog-block-id":
					b.Id = ""
				case "catalog-block-version":
					b.V = 0
				case "catalog-lock-reason":
					b.Status = "locked"
					b.Lock = &commonv1.BlockLock{}
				case "catalog-tiles-kind":
					b.Data.Fields["tiles"] = structpb.NewStringValue("invalid")
				case "catalog-tile-kind":
					b.Data.Fields["tiles"].GetListValue().Values[0] = structpb.NewStringValue("invalid")
				case "catalog-tile-id":
					tile.Fields["id"] = structpb.NewStringValue("")
				case "catalog-tile-status":
					tile.Fields["status"] = structpb.NewStringValue("future")
				case "catalog-tile-lock":
					tile.Fields["status"] = structpb.NewStringValue("locked")
					tile.Fields["lock"] = structpb.NewStringValue("private")
				case "catalog-integer-precision":
					tile.Fields["value"] = structpb.NewNumberValue(9007199254740992)
				}
			}
			model := generationAPIModel(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, generationAPIFunctionID("read", tool, args))
			})
			env := generationAPIEnv(t, peers, model)
			accepted := generationAPIAsk(t, env, generationAPIUser, generationAPIKey(), "Покажи финансовые данные", commonv1.TransferMode_TRANSFER_MODE_WITHOUT)
			presenterAssertFailure(t, env, generationAPIUser, accepted.AnswerId)
			if model.count.Load() != 1 {
				t.Fatal("untrusted financial evidence reached a model continuation")
			}
		})
	}
}
