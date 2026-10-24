package client

import (
	"errors"
	"math"
	"testing"

	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	statementv1 "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/fin-assist/assistant/internal/tooldata"
)

func TestUnitFinancialCatalogRejectsMalformedStatusesAndValues(t *testing.T) {
	for name, mutate := range map[string]func(*commonv1.Block){
		"unknown state":  func(b *commonv1.Block) { b.Status = "future" },
		"unknown schema": func(b *commonv1.Block) { b.V = 2 },
		"missing data":   func(b *commonv1.Block) { b.Data = nil },
		"missing tiles":  func(b *commonv1.Block) { delete(b.Data.Fields, "tiles") },
		"tiles not list": func(b *commonv1.Block) { b.Data.Fields["tiles"] = structpb.NewStringValue("invalid") },
		"tile not object": func(b *commonv1.Block) {
			b.Data.Fields["tiles"] = structpb.NewListValue(&structpb.ListValue{Values: []*structpb.Value{structpb.NewStringValue("invalid")}})
		},
		"tile missing id": func(b *commonv1.Block) {
			b.Data = financialStruct(t, map[string]any{"tiles": []any{map[string]any{"status": "locked"}}})
		},
		"tile unknown status": func(b *commonv1.Block) {
			b.Data = financialStruct(t, map[string]any{"tiles": []any{map[string]any{"id": "x", "status": "future"}}})
		},
		"tile non-string status": func(b *commonv1.Block) {
			b.Data = financialStruct(t, map[string]any{"tiles": []any{map[string]any{"id": "x", "status": 1}}})
		},
		"tile missing lock": func(b *commonv1.Block) {
			b.Data = financialStruct(t, map[string]any{"tiles": []any{map[string]any{"id": "x", "status": "locked"}}})
		},
		"tile wrong lock": func(b *commonv1.Block) {
			b.Data = financialStruct(t, map[string]any{"tiles": []any{map[string]any{"id": "x", "status": "locked", "lock": "no"}}})
		},
		"top missing lock":    func(b *commonv1.Block) { b.Status = "locked" },
		"top empty lock text": func(b *commonv1.Block) { b.Status = "locked"; b.Lock = &commonv1.BlockLock{} },
		"imprecise amount": func(b *commonv1.Block) {
			b.Data = financialStruct(t, map[string]any{"tiles": []any{map[string]any{"id": "x", "value_kind": "amount", "value": 9007199254740992.0}}})
		},
		"fractional amount": func(b *commonv1.Block) {
			b.Data = financialStruct(t, map[string]any{"tiles": []any{map[string]any{"id": "x", "value_kind": "amount", "value": 1.5}}})
		},
		"unknown value kind": func(b *commonv1.Block) {
			b.Data = financialStruct(t, map[string]any{"tiles": []any{map[string]any{"id": "x", "value_kind": "future", "value": 1}}})
		},
		"wrong value kind type": func(b *commonv1.Block) {
			b.Data = financialStruct(t, map[string]any{"tiles": []any{map[string]any{"id": "x", "value_kind": 1, "value": 1}}})
		},
		"NaN": func(b *commonv1.Block) {
			b.Data = financialStruct(t, map[string]any{"tiles": []any{map[string]any{"id": "x", "value_kind": "percent", "value": math.NaN()}}})
		},
		"Inf": func(b *commonv1.Block) {
			b.Data = financialStruct(t, map[string]any{"tiles": []any{map[string]any{"id": "x", "value_kind": "percent", "value": math.Inf(1)}}})
		},
	} {
		t.Run(name, func(t *testing.T) {
			c, f, s := financialFixture(t)
			b := &commonv1.Block{Id: "tiles", Type: "tile_group", V: 1, Data: financialStruct(t, map[string]any{"tiles": []any{map[string]any{"id": "expense", "value_kind": "amount", "value": 100}}})}
			mutate(b)
			f.analytics.Blocks = []*commonv1.Block{b}
			if _, err := c.Analytics(t.Context(), s, tooldata.AnalyticsArgs{}); !errors.Is(err, tooldata.ErrInvalidResponse) {
				t.Fatalf("err %v", err)
			}
		})
	}
	for _, value := range []map[string]any{
		{"value_kind": "amount", "value": -9007199254740991.0},
		{"value_kind": "count", "value": 0},
		{"value_kind": "percent", "value": 0.25},
		{"value_kind": "amount", "summary": map[string]any{"value": 100}},
		{"value_kind": "amount", "points": []any{map[string]any{"value": 100}, map[string]any{"value": nil}}},
	} {
		if !financialNumbers(financialStruct(t, value), "") {
			t.Fatalf("valid catalog value rejected %+v", value)
		}
	}
}

func TestUnitFinancialGroupKeysAndCollectionCaps(t *testing.T) {
	for name, keys := range map[string][]*statementv1.GroupKey{
		"month":    {{Month: financialOptional("2026-09")}},
		"category": {{Category: &statementv1.CategoryRef{Id: "cat"}}},
		"merchant": {{Merchant: &statementv1.MerchantRef{Selection: "s_merchant_test"}}},
	} {
		t.Run(name, func(t *testing.T) {
			c, f, s := financialFixture(t)
			f.aggregate.Groups[0].Keys = keys
			r, err := c.Aggregate(t.Context(), s, tooldata.AggregateArgs{GroupBy: []string{name}})
			if err != nil || r.Evidence.NoData {
				t.Fatalf("%+v %v", r, err)
			}
			f.aggregate.Groups[0].Keys = []*statementv1.GroupKey{{Month: financialOptional("2026-99"), Category: &statementv1.CategoryRef{Id: "cat"}}}
			if _, err := c.Aggregate(t.Context(), s, tooldata.AggregateArgs{GroupBy: []string{name}}); !errors.Is(err, tooldata.ErrInvalidResponse) {
				t.Fatal(err)
			}
		})
	}
	c, f, s := financialFixture(t)
	f.analytics.Blocks[0].Data = financialStruct(t, map[string]any{"items": make([]any, 4)})
	c.limits.CollectionItems = 3
	if _, err := c.Analytics(t.Context(), s, tooldata.AnalyticsArgs{}); !errors.Is(err, tooldata.ErrResultTooLarge) {
		t.Fatalf("nested list cap: %v", err)
	}
	c, f, s = financialFixture(t)
	f.analytics.Blocks[0].Data = financialStruct(t, map[string]any{"a": 1, "b": 2, "c": 3, "d": 4})
	c.limits.CollectionItems = 3
	if _, err := c.Analytics(t.Context(), s, tooldata.AnalyticsArgs{}); !errors.Is(err, tooldata.ErrResultTooLarge) {
		t.Fatalf("nested map cap: %v", err)
	}
	c, f, s = financialFixture(t)
	f.periods.Periods = append(f.periods.Periods, f.periods.Periods[0])
	c.limits.CollectionItems = 1
	if _, err := c.Periods(t.Context(), s, tooldata.PeriodsArgs{}); !errors.Is(err, tooldata.ErrResultTooLarge) {
		t.Fatalf("period count cap: %v", err)
	}
}
