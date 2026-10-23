package client

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	statementv1 "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/fin-assist/assistant/internal/tooldata"
)

type financialFake struct {
	statementv1.AssistantToolsServiceClient
	analyticsv1.AnalyticsServiceClient
	overview  *statementv1.GetDataOverviewResponse
	aggregate *statementv1.AggregateOperationsResponse
	find      *statementv1.FindOperationsResponse
	analytics *analyticsv1.GetAnalyticsResponse
	periods   *analyticsv1.ListPeriodsResponse
	err       error
	requests  []proto.Message
	deadlines []time.Duration
}

func (f *financialFake) record(ctx context.Context, r proto.Message) {
	f.requests = append(f.requests, r)
	deadline, ok := ctx.Deadline()
	if !ok {
		panic("missing deadline")
	}
	f.deadlines = append(f.deadlines, time.Until(deadline))
}
func (f *financialFake) GetDataOverview(ctx context.Context, r *statementv1.GetDataOverviewRequest, _ ...grpc.CallOption) (*statementv1.GetDataOverviewResponse, error) {
	f.record(ctx, r)
	return f.overview, f.err
}
func (f *financialFake) AggregateOperations(ctx context.Context, r *statementv1.AggregateOperationsRequest, _ ...grpc.CallOption) (*statementv1.AggregateOperationsResponse, error) {
	f.record(ctx, r)
	return f.aggregate, f.err
}
func (f *financialFake) FindOperations(ctx context.Context, r *statementv1.FindOperationsRequest, _ ...grpc.CallOption) (*statementv1.FindOperationsResponse, error) {
	f.record(ctx, r)
	return f.find, f.err
}
func (f *financialFake) GetAnalytics(ctx context.Context, r *analyticsv1.GetAnalyticsRequest, _ ...grpc.CallOption) (*analyticsv1.GetAnalyticsResponse, error) {
	f.record(ctx, r)
	return f.analytics, f.err
}
func (f *financialFake) ListPeriods(ctx context.Context, r *analyticsv1.ListPeriodsRequest, _ ...grpc.CallOption) (*analyticsv1.ListPeriodsResponse, error) {
	f.record(ctx, r)
	return f.periods, f.err
}

func financialFixture(t *testing.T) (*FinancialData, *financialFake, tooldata.Subject) {
	t.Helper()
	start := timestamppb.New(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	end := timestamppb.New(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	rangeValue := &commonv1.TimeRange{Start: start, End: end}
	bounds := &commonv1.DataBounds{Coverage: commonv1.Coverage_COVERAGE_COMPLETE, DataFrom: start, DataTo: timestamppb.New(end.AsTime().Add(-time.Second))}
	version := &statementv1.DataVersion{CategoriesChangedAt: start, UploadsChangedAt: end}
	f := &financialFake{
		overview:  &statementv1.GetDataOverviewResponse{HasData: true, DataFrom: start, DataTo: end, FullMonths: 1, Months: []*statementv1.MonthCoverage{{Month: "2026-09", Range: rangeValue, Bounds: bounds}}, SuppliedFields: []string{"posted_at"}, SourceNames: []string{"bank"}, Version: version, TimeZone: "Europe/Moscow", CalculatedAt: end},
		aggregate: &statementv1.AggregateOperationsResponse{Groups: []*statementv1.AggregateGroup{{AmountMinor: math.MaxInt64, OperationCount: 1, AverageMinor: math.MaxInt64, Filters: &commonv1.OperationsFilter{TransferMode: commonv1.TransferMode_TRANSFER_MODE_WITHOUT}}}, TotalAmountMinor: math.MaxInt64, TotalOperationCount: 1, Range: rangeValue, Bounds: bounds, Version: version, CalculatedAt: end, TimeZone: "Europe/Moscow"},
		find:      &statementv1.FindOperationsResponse{Items: []*statementv1.Operation{{Id: "op", Kind: statementv1.OperationKind_OPERATION_KIND_EXPENSE, Status: statementv1.OperationStatus_OPERATION_STATUS_POSTED, OccurredAt: start, AmountMinor: math.MaxInt64, Currency: "RUB", Title: "bank operation"}}, TotalCount: 2, Range: rangeValue, Bounds: bounds, Version: version, TimeZone: "Europe/Moscow"},
		analytics: &analyticsv1.GetAnalyticsResponse{HasData: true, Params: &commonv1.AnalyticsParams{Period: commonv1.PeriodType_PERIOD_TYPE_MONTH, PeriodKey: "2026-09", TransferMode: commonv1.TransferMode_TRANSFER_MODE_WITHOUT}, Period: &analyticsv1.PeriodInfo{Range: rangeValue, Bounds: bounds}, State: &analyticsv1.ScreenState{DataFrom: start, DataTo: end, CalculatedAt: end, FullMonths: 1}, TimeZone: "Europe/Moscow", Currency: "RUB", Blocks: []*commonv1.Block{{Id: "expenses", Type: "text", V: 1, Data: financialStruct(t, map[string]any{"text": "Expenses"})}}},
		periods:   &analyticsv1.ListPeriodsResponse{Periods: []*analyticsv1.PeriodItem{{Key: "2026-09", Range: rangeValue, Coverage: commonv1.Coverage_COVERAGE_COMPLETE}}, TimeZone: "Europe/Moscow"},
	}
	c, err := NewFinancialData(f, f, FinancialLimits{ResultBytes: 100_000, CollectionItems: 1000})
	if err != nil {
		t.Fatal(err)
	}
	return c, f, tooldata.Subject{UserID: "verified-user", TransferMode: "without"}
}
func financialStruct(t *testing.T, value map[string]any) *structpb.Struct {
	t.Helper()
	result, err := structpb.NewStruct(value)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestFinancialDataMainPathsAndExactIntegers(t *testing.T) {
	c, f, subject := financialFixture(t)
	start := f.overview.DataFrom.AsTime()
	end := f.overview.DataTo.AsTime()
	zero := int64(0)
	max := int64(math.MaxInt64)
	filter := tooldata.Filter{From: &start, To: &end, Query: "merchant", CategoryID: "cat", Selection: "s_merchant_test", Kind: "expense", AmountFrom: &zero, AmountTo: &max}
	results := make([]tooldata.Result, 0, 5)
	overview, err := c.Overview(t.Context(), subject)
	if err != nil {
		t.Fatal(err)
	}
	results = append(results, overview)
	aggregate, err := c.Aggregate(t.Context(), subject, tooldata.AggregateArgs{Filter: filter})
	if err != nil {
		t.Fatal(err)
	}
	results = append(results, aggregate)
	find, err := c.Find(t.Context(), subject, tooldata.FindArgs{Filter: filter, Order: "largest", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	results = append(results, find)
	analytics, err := c.Analytics(t.Context(), subject, tooldata.AnalyticsArgs{PeriodKey: "2026-09"})
	if err != nil {
		t.Fatal(err)
	}
	results = append(results, analytics)
	periods, err := c.Periods(t.Context(), subject, tooldata.PeriodsArgs{})
	if err != nil {
		t.Fatal(err)
	}
	results = append(results, periods)
	for i, result := range results {
		if !json.Valid(result.JSON) || result.Evidence.NoData || result.Evidence.Source.TimeZone != "Europe/Moscow" {
			t.Fatalf("result[%d]: %+v", i, result)
		}
		if f.deadlines[i] <= 0 || f.deadlines[i] > 5*time.Second && i != 3 || f.deadlines[i] > 10*time.Second {
			t.Fatalf("deadline[%d]: %s", i, f.deadlines[i])
		}
	}
	if !strings.Contains(string(aggregate.JSON), `"9223372036854775807"`) || !strings.Contains(string(find.JSON), `"amount_minor"`) {
		t.Fatalf("lost int64 or snake_case: %s %s", aggregate.JSON, find.JSON)
	}
	if !find.Evidence.Truncated || aggregate.Evidence.Truncated || find.Evidence.Source.CalculatedAt != nil || !aggregate.Evidence.Source.Version.CategoriesChangedAt.Equal(start) || !overview.Evidence.Source.Range.End.Equal(end) {
		t.Fatalf("metadata %+v %+v %+v", find.Evidence, aggregate.Evidence, overview.Evidence)
	}
	// DataBounds.data_to remains an inclusive-day display moment in JSON, and
	// provenance range.end remains the separate exclusive boundary.
	if !strings.Contains(string(aggregate.JSON), "2026-09-30T23:59:59Z") || !aggregate.Evidence.Source.Range.End.Equal(end) {
		t.Fatal("coverage boundary reinterpreted")
	}
	r := f.requests[1].(*statementv1.AggregateOperationsRequest)
	if r.UserId != subject.UserID || r.Limit != 50 || r.Filter.TransferMode != commonv1.TransferMode_TRANSFER_MODE_WITHOUT || r.Filter.GetQ() != "merchant" || r.Filter.GetSelection() != "s_merchant_test" || r.Filter.Kind != commonv1.KindFilter_KIND_FILTER_EXPENSE || r.Filter.GetAmountToMinor() != math.MaxInt64 || !r.Filter.From.AsTime().Equal(start) {
		t.Fatalf("aggregate request %+v", r)
	}
	rf := f.requests[2].(*statementv1.FindOperationsRequest)
	if rf.Order != statementv1.ToolSortOrder_TOOL_SORT_ORDER_LARGEST || rf.Limit != 1 || rf.UserId != subject.UserID {
		t.Fatalf("find request %+v", rf)
	}
	ra := f.requests[3].(*analyticsv1.GetAnalyticsRequest)
	if ra.GetUserId() != subject.UserID || ra.GetPeriodKey() != "2026-09" || ra.TransferMode != commonv1.TransferMode_TRANSFER_MODE_WITHOUT {
		t.Fatalf("analytics request %+v", ra)
	}
}

func TestFinancialDataNoDataUsesCoverageNotZeroResults(t *testing.T) {
	c, f, subject := financialFixture(t)
	f.aggregate.Groups = nil
	f.aggregate.TotalOperationCount = 0
	f.find.Items = nil
	f.find.TotalCount = 0
	for name, call := range map[string]func() (tooldata.Result, error){"aggregate": func() (tooldata.Result, error) { return c.Aggregate(t.Context(), subject, tooldata.AggregateArgs{}) }, "find": func() (tooldata.Result, error) { return c.Find(t.Context(), subject, tooldata.FindArgs{}) }} {
		t.Run(name, func(t *testing.T) {
			r, err := call()
			if err != nil || r.Evidence.NoData {
				t.Fatalf("zero is not no_data: %+v %v", r, err)
			}
		})
	}
	f.aggregate.Bounds = &commonv1.DataBounds{Coverage: commonv1.Coverage_COVERAGE_NO_DATA}
	f.find.Bounds = f.aggregate.Bounds
	f.overview.HasData = false
	f.analytics = &analyticsv1.GetAnalyticsResponse{}
	f.periods.Periods = nil
	for name, call := range financialCalls(c, subject) {
		t.Run(name, func(t *testing.T) {
			r, err := call(t.Context())
			if err != nil || !r.Evidence.NoData {
				t.Fatalf("no_data: %+v %v", r, err)
			}
		})
	}
}

func financialCalls(c *FinancialData, s tooldata.Subject) map[string]func(context.Context) (tooldata.Result, error) {
	return map[string]func(context.Context) (tooldata.Result, error){
		"overview": func(ctx context.Context) (tooldata.Result, error) { return c.Overview(ctx, s) },
		"aggregate": func(ctx context.Context) (tooldata.Result, error) {
			return c.Aggregate(ctx, s, tooldata.AggregateArgs{})
		},
		"find": func(ctx context.Context) (tooldata.Result, error) { return c.Find(ctx, s, tooldata.FindArgs{}) },
		"analytics": func(ctx context.Context) (tooldata.Result, error) {
			return c.Analytics(ctx, s, tooldata.AnalyticsArgs{})
		},
		"periods": func(ctx context.Context) (tooldata.Result, error) { return c.Periods(ctx, s, tooldata.PeriodsArgs{}) },
	}
}

func TestFinancialDataInvalidArgumentsNeverCallPeer(t *testing.T) {
	c, f, subject := financialFixture(t)
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	earlier := at.Add(-time.Second)
	negative := int64(-1)
	two := int64(2)
	one := int64(1)
	zeroTime := time.Time{}
	for name, args := range map[string]tooldata.AggregateArgs{
		"negative limit": {Limit: -1}, "large limit": {Limit: 201}, "many groups": {GroupBy: []string{"month", "category", "merchant"}}, "duplicate groups": {GroupBy: []string{"month", "month"}}, "unknown group": {GroupBy: []string{"future"}}, "all_time and from": {AllTime: true, Filter: tooldata.Filter{From: &at}}, "reversed times": {Filter: tooldata.Filter{From: &at, To: &earlier}}, "equal times": {Filter: tooldata.Filter{From: &at, To: &at}}, "zero time": {Filter: tooldata.Filter{From: &zeroTime}}, "negative amount": {Filter: tooldata.Filter{AmountFrom: &negative}}, "negative maximum": {Filter: tooldata.Filter{AmountTo: &negative}}, "reversed amount": {Filter: tooldata.Filter{AmountFrom: &two, AmountTo: &one}}, "unknown kind": {Filter: tooldata.Filter{Kind: "future"}}, "invalid UTF8": {Filter: tooldata.Filter{Query: string([]byte{255})}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := c.Aggregate(t.Context(), subject, args); !errors.Is(err, tooldata.ErrInvalidArguments) {
				t.Fatal(err)
			}
		})
	}
	for name, args := range map[string]tooldata.FindArgs{"negative limit": {Limit: -1}, "large limit": {Limit: 101}, "unknown order": {Order: "future"}, "bad filter": {AllTime: true, Filter: tooldata.Filter{To: &at}}} {
		t.Run("find "+name, func(t *testing.T) {
			if _, err := c.Find(t.Context(), subject, args); !errors.Is(err, tooldata.ErrInvalidArguments) {
				t.Fatal(err)
			}
		})
	}
	for _, args := range []tooldata.AnalyticsArgs{{Period: "future"}, {PeriodKey: "2026-13"}, {Period: "month", PeriodKey: "2026-Q3"}, {Period: "quarter", PeriodKey: "2026"}, {Period: "year", PeriodKey: "0000"}} {
		if _, err := c.Analytics(t.Context(), subject, args); !errors.Is(err, tooldata.ErrInvalidArguments) {
			t.Fatal(err)
		}
	}
	if _, err := c.Periods(t.Context(), subject, tooldata.PeriodsArgs{Period: "future"}); !errors.Is(err, tooldata.ErrInvalidArguments) {
		t.Fatal(err)
	}
	for _, subject := range []tooldata.Subject{{UserID: "", TransferMode: "with"}, {UserID: "  ", TransferMode: "with"}, {UserID: "u", TransferMode: "future"}, {UserID: "u"}} {
		for name, call := range financialCalls(c, subject) {
			t.Run("subject "+name, func(t *testing.T) {
				if _, err := call(t.Context()); !errors.Is(err, tooldata.ErrInvalidArguments) {
					t.Fatal(err)
				}
			})
		}
	}
	if len(f.requests) != 0 {
		t.Fatalf("invalid args reached peer: %+v", f.requests)
	}
}

func TestFinancialDataAlternativeRequests(t *testing.T) {
	c, f, subject := financialFixture(t)
	f.aggregate.Groups = nil
	for _, groups := range [][]string{nil, {"month"}, {"category", "merchant"}} {
		if _, err := c.Aggregate(t.Context(), subject, tooldata.AggregateArgs{AllTime: true, GroupBy: groups, Limit: 200, Filter: tooldata.Filter{Kind: "income"}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.Find(t.Context(), subject, tooldata.FindArgs{AllTime: true, Limit: 100, Order: "newest"}); err != nil {
		t.Fatal(err)
	}
	if got := f.requests[len(f.requests)-1].(*statementv1.FindOperationsRequest); !got.AllTime || got.Order != statementv1.ToolSortOrder_TOOL_SORT_ORDER_NEWEST {
		t.Fatal(got)
	}
	f.analytics.Params.Period = commonv1.PeriodType_PERIOD_TYPE_QUARTER
	f.analytics.Params.PeriodKey = "2026-Q3"
	if _, err := c.Analytics(t.Context(), subject, tooldata.AnalyticsArgs{Period: "quarter", PeriodKey: "2026-09"}); err != nil {
		t.Fatal(err)
	}
	f.analytics.Params.Period = commonv1.PeriodType_PERIOD_TYPE_YEAR
	f.analytics.Params.PeriodKey = "2026"
	for _, key := range []string{"2026-09", "2026-Q3", "2026"} {
		if _, err := c.Analytics(t.Context(), subject, tooldata.AnalyticsArgs{Period: "year", PeriodKey: key}); err != nil {
			t.Fatal(err)
		}
	}
	f.periods.Periods[0].Key = "2026"
	if _, err := c.Periods(t.Context(), subject, tooldata.PeriodsArgs{Period: "year"}); err != nil {
		t.Fatal(err)
	}
}

func TestFinancialDataPeerFailuresAndDeadlines(t *testing.T) {
	for name := range financialCalls(nil, tooldata.Subject{}) {
		t.Run(name, func(t *testing.T) {
			c, f, subject := financialFixture(t)
			f.err = errors.New("PRIVATE SQL DSN or user data")
			call := financialCalls(c, subject)[name]
			if r, err := call(t.Context()); !errors.Is(err, tooldata.ErrUnavailable) || len(r.JSON) != 0 {
				t.Fatalf("result %+v; err %v", r, err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if _, err := call(ctx); !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			ctx, cancel = context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
			defer cancel()
			if _, err := call(ctx); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal(err)
			}
			f.err = nil
			ctx, cancel = context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			if _, err := call(ctx); err != nil {
				t.Fatal(err)
			}
			if f.deadlines[len(f.deadlines)-1] > time.Second {
				t.Fatal("caller deadline extended")
			}
			unconfigured, err := NewFinancialData(nil, nil, c.limits)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := financialCalls(unconfigured, subject)[name](t.Context()); !errors.Is(err, tooldata.ErrUnavailable) {
				t.Fatal(err)
			}
		})
	}
	for _, limits := range []FinancialLimits{{}, {ResultBytes: 1}, {CollectionItems: 1}, {ResultBytes: -1, CollectionItems: 1}} {
		if _, err := NewFinancialData(nil, nil, limits); !errors.Is(err, tooldata.ErrInvalidArguments) {
			t.Fatal(err)
		}
	}
}

func TestFinancialDataMalformedResponsesAndLimits(t *testing.T) {
	for name, mutate := range map[string]func(*financialFake){
		"overview nil": func(f *financialFake) { f.overview = nil }, "overview missing calculation": func(f *financialFake) { f.overview.CalculatedAt = nil }, "overview bad timezone": func(f *financialFake) { f.overview.TimeZone = "Local" }, "overview reversed data bounds": func(f *financialFake) { f.overview.DataTo = f.overview.DataFrom }, "overview missing bounds": func(f *financialFake) { f.overview.DataTo = nil }, "overview bad month": func(f *financialFake) { f.overview.Months[0].Month = "2026-99" }, "overview unknown coverage": func(f *financialFake) { f.overview.Months[0].Bounds.Coverage = commonv1.Coverage(99) }, "overview bad version timestamp": func(f *financialFake) {
			f.overview.Version.UploadsChangedAt = &timestamppb.Timestamp{Seconds: math.MaxInt64}
		},
		"aggregate missing bounds": func(f *financialFake) { f.aggregate.Bounds = nil }, "aggregate negative count": func(f *financialFake) { f.aggregate.Groups[0].OperationCount = -1 }, "aggregate wrong transfer mode": func(f *financialFake) {
			f.aggregate.Groups[0].Filters.TransferMode = commonv1.TransferMode_TRANSFER_MODE_WITH
		}, "aggregate keys mismatch": func(f *financialFake) { f.aggregate.Groups[0].Keys = []*statementv1.GroupKey{{}} }, "aggregate bad range": func(f *financialFake) { f.aggregate.Range.End = f.aggregate.Range.Start },
		"find invalid amount": func(f *financialFake) { f.find.Items[0].AmountMinor = -1 }, "find missing timestamp": func(f *financialFake) { f.find.Items[0].OccurredAt = nil }, "find unknown kind": func(f *financialFake) { f.find.Items[0].Kind = statementv1.OperationKind(99) }, "find unknown status": func(f *financialFake) { f.find.Items[0].Status = statementv1.OperationStatus(99) }, "find bad currency": func(f *financialFake) { f.find.Items[0].Currency = "rub" }, "find smaller total": func(f *financialFake) { f.find.TotalCount = 0 },
		"analytics nil": func(f *financialFake) { f.analytics = nil }, "analytics unknown period": func(f *financialFake) { f.analytics.Params.Period = commonv1.PeriodType(99) }, "analytics wrong mode": func(f *financialFake) { f.analytics.Params.TransferMode = commonv1.TransferMode_TRANSFER_MODE_WITH }, "analytics no_data with fields": func(f *financialFake) { f.analytics.HasData = false }, "analytics bad currency": func(f *financialFake) { f.analytics.Currency = "" }, "analytics missing calculation": func(f *financialFake) { f.analytics.State.CalculatedAt = nil }, "analytics unknown block status": func(f *financialFake) { f.analytics.Blocks[0].Status = "future" },
		"periods unknown coverage": func(f *financialFake) { f.periods.Periods[0].Coverage = commonv1.Coverage(99) }, "periods no_data coverage": func(f *financialFake) { f.periods.Periods[0].Coverage = commonv1.Coverage_COVERAGE_NO_DATA }, "periods bad key": func(f *financialFake) { f.periods.Periods[0].Key = "2026" },
	} {
		t.Run(name, func(t *testing.T) {
			c, f, s := financialFixture(t)
			mutate(f)
			method, _, _ := strings.Cut(name, " ")
			if _, err := financialCalls(c, s)[method](t.Context()); !errors.Is(err, tooldata.ErrInvalidResponse) {
				t.Fatalf("err %v", err)
			}
		})
	}
	for name := range financialCalls(nil, tooldata.Subject{}) {
		t.Run(name+" byte cap", func(t *testing.T) {
			c, _, s := financialFixture(t)
			c.limits.ResultBytes = 1
			if _, err := financialCalls(c, s)[name](t.Context()); !errors.Is(err, tooldata.ErrResultTooLarge) {
				t.Fatal(err)
			}
		})
	}
	c, f, s := financialFixture(t)
	f.overview.Months = append(f.overview.Months, f.overview.Months[0])
	c.limits.CollectionItems = 1
	if _, err := c.Overview(t.Context(), s); !errors.Is(err, tooldata.ErrResultTooLarge) {
		t.Fatal(err)
	}
	c, f, s = financialFixture(t)
	f.aggregate.Groups = append(f.aggregate.Groups, f.aggregate.Groups[0])
	if _, err := c.Aggregate(t.Context(), s, tooldata.AggregateArgs{Limit: 1}); !errors.Is(err, tooldata.ErrInvalidResponse) {
		t.Fatal(err)
	}
	c, f, s = financialFixture(t)
	f.find.Items = append(f.find.Items, f.find.Items[0])
	if _, err := c.Find(t.Context(), s, tooldata.FindArgs{Limit: 1}); !errors.Is(err, tooldata.ErrInvalidResponse) {
		t.Fatal(err)
	}
}

func TestFinancialDataAnalyticsCatalogEvidenceAndProjection(t *testing.T) {
	c, f, s := financialFixture(t)
	locked := &commonv1.Block{Id: "forecast", Type: "chart", V: 1, Status: "locked", Lock: &commonv1.BlockLock{Reason: "future_reason", Text: "Need more data"}}
	tiles := &commonv1.Block{Id: "tiles", Type: "tile_group", V: 1, Data: financialStruct(t, map[string]any{"tiles": []any{map[string]any{"id": "expense", "status": "ready", "value_kind": "amount", "value": 100}, map[string]any{"id": "forecast", "status": "locked", "lock": map[string]any{}}, map[string]any{"id": "comparison", "status": "none"}}})}
	f.analytics.Blocks = []*commonv1.Block{locked, tiles}
	// Deprecated values deliberately disagree. They must neither leak into the
	// tool payload nor influence the catalog evidence.
	legacy := f.analytics.ProtoReflect().Descriptor().Fields().ByName("tiles")
	f.analytics.ProtoReflect().Mutable(legacy).Message().Set(f.analytics.ProtoReflect().Mutable(legacy).Message().Descriptor().Fields().ByName("expense"), protoreflect.ValueOfMessage((&analyticsv1.Metric{Status: analyticsv1.MetricStatus(99)}).ProtoReflect()))
	r, err := c.Analytics(t.Context(), s, tooldata.AnalyticsArgs{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.Evidence.LockedPaths, []string{"blocks[0]", "blocks[1].data.tiles[1]"}) {
		t.Fatalf("evidence %+v", r.Evidence)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(r.JSON, &payload); err != nil {
		t.Fatal(err)
	}
	if _, exists := payload["tiles"]; exists {
		t.Fatalf("retired metrics leaked %s", r.JSON)
	}
	if !f.analytics.ProtoReflect().Has(legacy) {
		t.Fatal("provider response was mutated")
	}
}
