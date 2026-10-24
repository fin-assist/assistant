package client

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"testing"

	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	statementv1 "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/fin-assist/assistant/internal/tooldata"
)

func TestFinancialFactsPreserveModelJSON(t *testing.T) {
	c, peer, subject := financialFixture(t)
	tests := []struct {
		name    string
		message proto.Message
		run     func(context.Context) (tooldata.Result, error)
	}{
		{"overview", peer.overview, func(ctx context.Context) (tooldata.Result, error) { return c.Overview(ctx, subject) }},
		{"aggregate", peer.aggregate, func(ctx context.Context) (tooldata.Result, error) {
			return c.Aggregate(ctx, subject, tooldata.AggregateArgs{})
		}},
		{"find", peer.find, func(ctx context.Context) (tooldata.Result, error) { return c.Find(ctx, subject, tooldata.FindArgs{}) }},
		{"analytics", peer.analytics, func(ctx context.Context) (tooldata.Result, error) {
			return c.Analytics(ctx, subject, tooldata.AnalyticsArgs{})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.run(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			expected, err := (protojson.MarshalOptions{UseProtoNames: true}).Marshal(tt.message)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got.JSON, expected) {
				t.Fatalf("model JSON changed: %s != %s", got.JSON, expected)
			}
			if got.Evidence.Source == nil || got.Evidence.Source.TimeZone != "Europe/Moscow" {
				t.Fatalf("source lost: %+v", got.Evidence)
			}
			switch tt.name {
			case "overview":
				f := got.Overview
				if f == nil || f.Zone != "Europe/Moscow" || len(f.Months) != 1 || f.Months[0].Bounds.Coverage != "complete" || f.Months[0].Month != "2026-09" || !f.Months[0].Range.Start.Equal(peer.overview.Months[0].Range.Start.AsTime()) {
					t.Fatalf("overview: %+v", f)
				}
			case "aggregate":
				f := got.Aggregate
				if f == nil || f.Amount != math.MaxInt64 || f.Groups[0].Average != math.MaxInt64 || f.Count != 1 || f.Groups[0].Filters.Mode != "without" {
					t.Fatalf("aggregate: %+v", f)
				}
			case "find":
				f := got.Operations
				if f == nil || f.Count != 2 || len(f.Items) != 1 || f.Items[0].Amount != math.MaxInt64 || f.Items[0].Kind != "expense" || f.Items[0].Currency != "RUB" {
					t.Fatalf("operations: %+v", f)
				}
			case "analytics":
				f := got.Analytics
				if f == nil || !f.HasData || f.Currency != "RUB" || f.Params.Period != "month" || f.Params.Mode != "without" || f.Params.Key != "2026-09" || string(f.Blocks[0].Data) != `{"text":"Expenses"}` {
					t.Fatalf("analytics: %+v", f)
				}
			}
		})
	}
}

func TestFinancialFactsCatalogActionsAndLocks(t *testing.T) {
	c, peer, subject := financialFixture(t)
	title, alt, empty := "Title", "Alternative", "Empty"
	amount := int64(9007199254740993)
	q := "shop"
	selection := "opaque"
	name := "Selection"
	filter := &commonv1.OperationsFilter{Q: &q, Selection: &selection, SelectionName: &name, AmountFromMinor: &amount, Kind: commonv1.KindFilter_KIND_FILTER_INCOME, TransferMode: commonv1.TransferMode_TRANSFER_MODE_WITHOUT}
	peer.analytics.Blocks = []*commonv1.Block{{Id: "locked", Type: "rows", V: 3, Status: "locked", Lock: &commonv1.BlockLock{Text: "Need full months"}, Title: &title, AltText: &alt, EmptyText: &empty, Data: financialStruct(t, map[string]any{"value_kind": "count", "value": 9007199254740991.0}), Action: &commonv1.BlockAction{Type: "open_operations", Filter: filter}}}
	got, err := c.Analytics(t.Context(), subject, tooldata.AnalyticsArgs{})
	if err != nil {
		t.Fatal(err)
	}
	block := got.Analytics.Blocks[0]
	if block.ID != "locked" || block.V != 3 || block.Status != "locked" || block.Lock.Text != "Need full months" || block.Title != title || block.Alt != alt || block.Empty != empty || len(got.Evidence.LockedPaths) != 1 {
		t.Fatalf("catalog metadata: %+v", block)
	}
	if block.Action == nil || block.Action.Mode != "without" || block.Action.Kind != "income" || *block.Action.AmountFrom != amount || block.Action.Selection != selection || block.Action.SelectionName != name {
		t.Fatalf("action: %+v", block.Action)
	}
	var data struct {
		Value json.Number `json:"value"`
	}
	if json.Unmarshal(block.Data, &data) != nil || data.Value.String() != "9007199254740991" {
		t.Fatalf("struct number lost: %s", block.Data)
	}
	amount = 1
	peer.analytics.Blocks[0].Lock.Text = "mutated"
	if *block.Action.AmountFrom != 9007199254740993 || block.Lock.Text != "Need full months" {
		t.Fatal("facts alias protobuf response")
	}
	for _, action := range []*commonv1.BlockAction{nil, {Type: "unknown", Filter: filter}, {Type: "open_operations"}, {Type: "open_operations", Filter: &commonv1.OperationsFilter{TransferMode: commonv1.TransferMode_TRANSFER_MODE_WITHOUT, SelectionName: &name}}, {Type: "open_operations", Filter: &commonv1.OperationsFilter{Q: &q, AmountFromMinor: proto.Int64(10), AmountToMinor: proto.Int64(1), TransferMode: commonv1.TransferMode_TRANSFER_MODE_WITHOUT}}} {
		peer.analytics.Blocks[0].Action = action
		got, err := c.Analytics(t.Context(), subject, tooldata.AnalyticsArgs{})
		if err != nil {
			t.Fatal(err)
		}
		if got.Analytics.Blocks[0].Action != nil {
			t.Fatalf("invalid action survived: %+v", action)
		}
	}
}

func TestFinancialFactsNormalizedEnums(t *testing.T) {
	c, peer, subject := financialFixture(t)
	for kind, want := range map[statementv1.OperationKind]string{statementv1.OperationKind_OPERATION_KIND_EXPENSE: "expense", statementv1.OperationKind_OPERATION_KIND_INCOME: "income", statementv1.OperationKind_OPERATION_KIND_OWN_TRANSFER: "own_transfer"} {
		peer.find.Items[0].Kind = kind
		peer.find.Items[0].Currency = "USD"
		got, err := c.Find(t.Context(), subject, tooldata.FindArgs{})
		if err != nil {
			t.Fatal(err)
		}
		if got.Operations.Items[0].Kind != want || got.Operations.Items[0].Currency != "USD" {
			t.Fatalf("operation normalization: %+v", got.Operations)
		}
	}
	for _, mode := range []commonv1.TransferMode{commonv1.TransferMode_TRANSFER_MODE_WITH, commonv1.TransferMode_TRANSFER_MODE_WITHOUT} {
		for kind, want := range map[commonv1.KindFilter]string{commonv1.KindFilter_KIND_FILTER_UNSPECIFIED: "", commonv1.KindFilter_KIND_FILTER_EXPENSE: "expense", commonv1.KindFilter_KIND_FILTER_INCOME: "income"} {
			f := financialNavigation(&commonv1.OperationsFilter{TransferMode: mode, Kind: kind})
			if f.Kind != want || f.Mode == "" {
				t.Fatalf("filter normalization: %+v", f)
			}
		}
	}
	for coverage, want := range map[commonv1.Coverage]string{commonv1.Coverage_COVERAGE_COMPLETE: "complete", commonv1.Coverage_COVERAGE_PARTIAL: "partial", commonv1.Coverage_COVERAGE_NO_DATA: "no_data"} {
		if financialCoverage(coverage) != want {
			t.Fatal("coverage normalization")
		}
	}
	for period, want := range map[commonv1.PeriodType]string{commonv1.PeriodType_PERIOD_TYPE_MONTH: "month", commonv1.PeriodType_PERIOD_TYPE_QUARTER: "quarter", commonv1.PeriodType_PERIOD_TYPE_YEAR: "year"} {
		if financialPeriodName(period) != want {
			t.Fatal("period normalization")
		}
	}
	peer.analytics.HasData = false
	peer.analytics.Params = nil
	peer.analytics.Period = nil
	peer.analytics.State = nil
	peer.analytics.Blocks = nil
	peer.analytics.Currency = ""
	peer.analytics.TimeZone = ""
	got, err := c.Analytics(t.Context(), subject, tooldata.AnalyticsArgs{})
	if err != nil || got.Analytics == nil || got.Analytics.HasData || !got.Evidence.NoData {
		t.Fatalf("no data: %+v %v", got, err)
	}
}

func TestFinancialFactsAnalyticsRealTransport(t *testing.T) {
	_, fixture, subject := financialFixture(t)
	fixture.analytics.Blocks[0].Status = "locked"
	fixture.analytics.Blocks[0].Lock = &commonv1.BlockLock{Text: "Trusted lock"}
	fixture.analytics.Blocks[0].Action = &commonv1.BlockAction{Type: "open_operations", Filter: &commonv1.OperationsFilter{CategoryId: proto.String("food"), TransferMode: commonv1.TransferMode_TRANSFER_MODE_WITHOUT}}
	peer := &financialPeer{analyticsResult: fixture.analytics}
	c := startFinancialPeer(t, peer)
	got, err := c.Analytics(t.Context(), subject, tooldata.AnalyticsArgs{Period: "month", PeriodKey: "2026-09"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Analytics == nil || got.Analytics.Params.Period != "month" || got.Analytics.Params.Mode != "without" || got.Analytics.Blocks[0].Action.CategoryID != "food" || got.Analytics.Blocks[0].Lock.Text != "Trusted lock" || len(got.Evidence.LockedPaths) != 1 {
		t.Fatalf("transport facts: %+v", got)
	}
	raw, err := (protojson.MarshalOptions{UseProtoNames: true}).Marshal(fixture.analytics)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.JSON, raw) {
		t.Fatalf("transport changed model JSON: %s", got.JSON)
	}
}

func TestFinancialFactsOverviewGapsAndGroupKeys(t *testing.T) {
	c, peer, subject := financialFixture(t)
	month := peer.overview.Months[0]
	month.Bounds.Coverage = commonv1.Coverage_COVERAGE_PARTIAL
	month.Bounds.Gaps = []*commonv1.TimeRange{proto.Clone(month.Range).(*commonv1.TimeRange)}
	got, err := c.Overview(t.Context(), subject)
	if err != nil {
		t.Fatal(err)
	}
	bounds := got.Overview.Months[0].Bounds
	if bounds.Coverage != "partial" || len(bounds.Gaps) != 1 || !bounds.Gaps[0].End.Equal(month.Range.End.AsTime()) || !bounds.DataFrom.Equal(month.Bounds.DataFrom.AsTime()) || !bounds.DataTo.Equal(month.Bounds.DataTo.AsTime()) {
		t.Fatalf("bounds lost: %+v", bounds)
	}
	peer.aggregate.Groups[0].Keys = []*statementv1.GroupKey{{Category: &statementv1.CategoryRef{Id: "food", Name: "Food"}}}
	got, err = c.Aggregate(t.Context(), subject, tooldata.AggregateArgs{GroupBy: []string{"category"}})
	if err != nil {
		t.Fatal(err)
	}
	key := got.Aggregate.Groups[0].Keys[0]
	if key.Category == nil || key.Category.ID != "food" || key.Category.Name != "Food" || key.Merchant != nil || key.Month != "" {
		t.Fatalf("group key lost: %+v", key)
	}
}

func TestFinancialNavigationRejectsUnknownFilterKind(t *testing.T) {
	filter := &commonv1.OperationsFilter{Q: proto.String("shop"), Kind: commonv1.KindFilter(1000), TransferMode: commonv1.TransferMode_TRANSFER_MODE_WITH}
	if financialNavigation(filter) != nil {
		t.Fatal("unknown kind broadened operation navigation")
	}
}
