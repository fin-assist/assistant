package generation

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fin-assist/assistant/internal/domain"
	"github.com/fin-assist/assistant/internal/tooldata"
)

func TestUnitPresentationUsesTrustedFactsIndependentlyOfModelJSON(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	for _, raw := range []json.RawMessage{nil, json.RawMessage(`not json`), json.RawMessage(`{"total_amount_minor":"999999999","groups":[{"filters":{"q":"untrusted"}}]}`)} {
		ref := ToolResultReference{
			CallID: "read", Name: "aggregate_operations", Args: tooldata.AggregateArgs{Filter: tooldata.Filter{Query: "trusted"}},
			Result: tooldata.Result{JSON: raw, Aggregate: &tooldata.Aggregate{Amount: 12345, Count: 2, Range: &tooldata.Range{Start: start, End: end}}},
		}
		out := presentationRun(t, ref, presentationArgs{View: "rows"})
		rows := presentationSavedRows(t, out)
		if rows[0].Value != json.Number("12345") || len(out.Chips) != 1 || !strings.Contains(string(out.Chips[0]), `"q":"trusted"`) || strings.Contains(string(out.Chips[0]), "untrusted") {
			t.Fatalf("model payload affected presentation: %+v", out)
		}
	}
}

func TestUnitPresentationRequiresTypedFactsForEachSupportedView(t *testing.T) {
	for _, tc := range []struct {
		name, view, path string
		args             any
	}{
		{"aggregate_operations", "rows", "", tooldata.AggregateArgs{}},
		{"aggregate_operations", "chart", "", tooldata.AggregateArgs{GroupBy: []string{"month"}}},
		{"find_operations", "rows", "", tooldata.FindArgs{}},
		{"get_analytics", "analytics", "blocks[0]", tooldata.AnalyticsArgs{}},
	} {
		ref := ToolResultReference{CallID: "read", Name: tc.name, Args: tc.args, Result: tooldata.Result{JSON: json.RawMessage(`{"total_amount_minor":1,"has_data":true,"blocks":[]}`)}}
		_, err := preparePresentation(domain.GenerationTask{TransferMode: "with"}, presentationArgs{ResultID: "read", View: tc.view, Path: tc.path}, map[string]ToolResultReference{"read": ref})
		if !errors.Is(err, errModelProtocol) {
			t.Fatalf("%s accepted missing facts: %v", tc.name, err)
		}
	}
}

func TestUnitPresentationAnalyticsActionUsesDefaultLabel(t *testing.T) {
	ref := ToolResultReference{CallID: "read", Name: "get_analytics", Args: tooldata.AnalyticsArgs{}, Result: tooldata.Result{
		Analytics: &tooldata.Analytics{HasData: true, Currency: "RUB", Params: tooldata.AnalyticsParams{Period: "month", Key: "2026-10", Mode: "with"}, Blocks: []tooldata.CatalogBlock{{Type: "text", V: 1, Data: json.RawMessage(`{"text":"Trusted fact"}`), Action: &tooldata.NavigationFilter{Mode: "with", Q: "shop"}}}},
	}}
	out := presentationRun(t, ref, presentationArgs{View: "analytics", Path: "blocks[0]"})
	if len(out.Chips) != 2 || !strings.Contains(string(out.Chips[0]), "Операции по выборке") {
		t.Fatalf("missing fallback label: %s", out.Chips)
	}
}
