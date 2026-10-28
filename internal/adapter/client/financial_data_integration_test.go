package client

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	grpcclient "github.com/fin-assist/go-grpc/client"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	statementv1 "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/fin-assist/go-grpc/reqctx"

	"github.com/fin-assist/assistant/internal/tooldata"
)

type financialCall struct {
	method    string
	req       proto.Message
	md        metadata.MD
	remaining time.Duration
}

type financialPeer struct {
	statementv1.UnimplementedAssistantToolsServiceServer
	analyticsv1.UnimplementedAnalyticsServiceServer
	mu              sync.Mutex
	calls           map[string]int
	seen            chan financialCall
	retryFirst      bool
	wait            bool
	failure         error
	analyticsResult *analyticsv1.GetAnalyticsResponse
}

func (p *financialPeer) record(ctx context.Context, method string, req proto.Message) error {
	p.mu.Lock()
	p.calls[method]++
	n := p.calls[method]
	p.mu.Unlock()
	md, _ := metadata.FromIncomingContext(ctx)
	d, _ := ctx.Deadline()
	p.seen <- financialCall{method: method, req: proto.Clone(req), md: md.Copy(), remaining: time.Until(d)}
	if p.wait {
		<-ctx.Done()
		return ctx.Err()
	}
	if p.failure != nil {
		return p.failure
	}
	if p.retryFirst && n == 1 {
		return status.Error(codes.Unavailable, "restarting")
	}
	return nil
}

func financialTransportRange() *commonv1.TimeRange {
	return &commonv1.TimeRange{Start: timestamppb.New(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)), End: timestamppb.New(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))}
}

func financialTransportBounds() *commonv1.DataBounds {
	r := financialTransportRange()
	return &commonv1.DataBounds{Coverage: commonv1.Coverage_COVERAGE_COMPLETE, DataFrom: r.Start, DataTo: timestamppb.New(r.End.AsTime().Add(-time.Second))}
}

func (p *financialPeer) GetDataOverview(ctx context.Context, q *statementv1.GetDataOverviewRequest) (*statementv1.GetDataOverviewResponse, error) {
	if err := p.record(ctx, "overview", q); err != nil {
		return nil, err
	}
	r := financialTransportRange()
	return &statementv1.GetDataOverviewResponse{HasData: true, DataFrom: r.Start, DataTo: r.End, FullMonths: 1, Months: []*statementv1.MonthCoverage{{Month: "2026-09", Range: r, Bounds: financialTransportBounds()}}, SuppliedFields: []string{"occurred_at", "amount_minor"}, SourceNames: []string{"test statement"}, Version: &statementv1.DataVersion{Revisions: strictWire(9223372036854775807)}, TimeZone: "Europe/Moscow", CalculatedAt: r.End}, nil
}

func (p *financialPeer) AggregateOperations(ctx context.Context, q *statementv1.AggregateOperationsRequest) (*statementv1.AggregateOperationsResponse, error) {
	if err := p.record(ctx, "aggregate", q); err != nil {
		return nil, err
	}
	month := "2026-09"
	groupFilter := proto.Clone(q.Filter).(*commonv1.OperationsFilter)
	selection := "s_merchant_token"
	groupFilter.Selection = &selection
	return &statementv1.AggregateOperationsResponse{Groups: []*statementv1.AggregateGroup{{Keys: []*statementv1.GroupKey{{Merchant: &statementv1.MerchantRef{Title: "Merchant", Selection: "s_merchant_token"}}, {Month: &month}}, AmountMinor: 9007199254740993, OperationCount: 2, AverageMinor: 4503599627370497, Filters: groupFilter}}, TotalAmountMinor: 9007199254741010, TotalOperationCount: 3, Range: financialTransportRange(), Bounds: financialTransportBounds(), Truncated: true, Version: &statementv1.DataVersion{Revisions: strictWire(9223372036854775807)}, CalculatedAt: financialTransportRange().End, TimeZone: "Europe/Moscow"}, nil
}

func (p *financialPeer) FindOperations(ctx context.Context, q *statementv1.FindOperationsRequest) (*statementv1.FindOperationsResponse, error) {
	if err := p.record(ctx, "find", q); err != nil {
		return nil, err
	}
	return &statementv1.FindOperationsResponse{Items: []*statementv1.Operation{{Id: "019be405-3b20-7000-8000-000000000001", Kind: statementv1.OperationKind_OPERATION_KIND_EXPENSE, OccurredAt: financialTransportRange().Start, AmountMinor: 9007199254740993, Currency: "RUB", Title: "test operation", Category: &statementv1.CategoryRef{Id: "category", Name: "Category", Icon: "other"}, Status: statementv1.OperationStatus_OPERATION_STATUS_POSTED, SourceName: "test statement"}}, TotalCount: 2, Range: financialTransportRange(), Bounds: financialTransportBounds(), Version: &statementv1.DataVersion{Revisions: strictWire(9223372036854775807)}, TimeZone: "Europe/Moscow"}, nil
}

func (p *financialPeer) GetAnalytics(ctx context.Context, q *analyticsv1.GetAnalyticsRequest) (*analyticsv1.GetAnalyticsResponse, error) {
	if err := p.record(ctx, "analytics", q); err != nil {
		return nil, err
	}
	if p.analyticsResult != nil {
		return proto.Clone(p.analyticsResult).(*analyticsv1.GetAnalyticsResponse), nil
	}
	return &analyticsv1.GetAnalyticsResponse{HasData: false, DataRevisions: strictWire(3)}, nil
}

func (p *financialPeer) ListPeriods(ctx context.Context, q *analyticsv1.ListPeriodsRequest) (*analyticsv1.ListPeriodsResponse, error) {
	if err := p.record(ctx, "periods", q); err != nil {
		return nil, err
	}
	periodRange := &commonv1.TimeRange{Start: timestamppb.New(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)), End: timestamppb.New(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))}
	return &analyticsv1.ListPeriodsResponse{Periods: []*analyticsv1.PeriodItem{{Key: "2026", Range: periodRange, Coverage: commonv1.Coverage_COVERAGE_COMPLETE}}, TimeZone: "Europe/Moscow", DataRevisions: strictWire(2)}, nil
}

func startFinancialPeer(t *testing.T, p *financialPeer) *FinancialData {
	t.Helper()
	p.calls = map[string]int{}
	p.seen = make(chan financialCall, 32)
	lis := bufconn.Listen(1 << 20)
	s := grpc.NewServer()
	statementv1.RegisterAssistantToolsServiceServer(s, p)
	analyticsv1.RegisterAnalyticsServiceServer(s, p)
	go func() { _ = s.Serve(lis) }()
	t.Cleanup(s.Stop)
	dial := func(peer string, retry []string) *grpc.ClientConn {
		c, err := grpcclient.Dial("passthrough:///"+peer, grpcclient.Options{Peer: peer, Tokens: grpcclient.DevToken("assistant"), Retry: retry, DialOptions: []grpc.DialOption{grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) })}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	stmt := dial("bank-statement-data", []string{"finassist.statementdata.v1.AssistantToolsService/GetDataOverview", "finassist.statementdata.v1.AssistantToolsService/AggregateOperations", "finassist.statementdata.v1.AssistantToolsService/FindOperations"})
	analytics := dial("analytics", []string{"finassist.analytics.v1.AnalyticsService/GetAnalytics", "finassist.analytics.v1.AnalyticsService/ListPeriods"})
	c, err := NewFinancialData(statementv1.NewAssistantToolsServiceClient(stmt), analyticsv1.NewAnalyticsServiceClient(analytics), FinancialLimits{ResultBytes: 1 << 20, CollectionItems: 200})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestIntegrationFinancialDataRealTransport(t *testing.T) {
	p := &financialPeer{retryFirst: true}
	c := startFinancialPeer(t, p)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	ctx = reqctx.WithRequestID(ctx, "financial-request")
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer user-secret", "x-read-primary", "1", "x-client-ip", "private-ip", "idempotency-key", "private-key")
	subject := tooldata.Subject{UserID: "019be405-3b20-7000-8000-000000000002", TransferMode: "without"}
	from, to := financialTransportRange().Start.AsTime(), financialTransportRange().End.AsTime()
	minAmount, maxAmount := int64(17), int64(9007199254740993)
	filter := tooldata.Filter{From: &from, To: &to, Query: "merchant", CategoryID: "category", Selection: "s_merchant_token", Kind: "expense", AmountFrom: &minAmount, AmountTo: &maxAmount}
	aggregateFilter := filter
	aggregateFilter.Selection = ""
	tests := []struct {
		name string
		run  func() (tooldata.Result, error)
	}{
		{"overview", func() (tooldata.Result, error) { return c.Overview(ctx, subject) }},
		{"aggregate", func() (tooldata.Result, error) {
			return c.Aggregate(ctx, subject, tooldata.AggregateArgs{Filter: aggregateFilter, GroupBy: []string{"merchant", "month"}, Limit: 1})
		}},
		{"find", func() (tooldata.Result, error) {
			return c.Find(ctx, subject, tooldata.FindArgs{Filter: filter, Order: "largest", Limit: 1})
		}},
		{"analytics", func() (tooldata.Result, error) {
			return c.Analytics(ctx, subject, tooldata.AnalyticsArgs{Period: "quarter", PeriodKey: "2026-09"})
		}},
		{"periods", func() (tooldata.Result, error) { return c.Periods(ctx, subject, tooldata.PeriodsArgs{Period: "year"}) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.run()
			if err != nil {
				t.Fatal(err)
			}
			want := int64(9223372036854775807)
			if tt.name == "analytics" {
				want = 3
			}
			if tt.name == "periods" {
				want = 2
			}
			if len(got.Evidence.Revisions) != 3 || got.Evidence.Revisions["uploads"] != want || strings.Contains(string(got.JSON), "revisions") {
				t.Fatalf("private revisions: %+v %s", got.Evidence, got.JSON)
			}
			switch tt.name {
			case "overview":
				if got.Overview == nil || got.Overview.Zone != "Europe/Moscow" || got.Overview.Months[0].Bounds.Coverage != "complete" {
					t.Fatalf("overview facts: %+v", got.Overview)
				}
			case "aggregate":
				f := got.Aggregate
				if f == nil || f.Amount != 9007199254741010 || f.Groups[0].Amount != 9007199254740993 || f.Groups[0].Keys[0].Merchant.Selection != "s_merchant_token" || f.Groups[0].Keys[1].Month != "2026-09" || f.Groups[0].Filters.Mode != "without" || *f.Groups[0].Filters.AmountTo != maxAmount {
					t.Fatalf("aggregate facts: %+v", f)
				}
			case "find":
				f := got.Operations
				if f == nil || f.Count != 2 || f.Items[0].Amount != 9007199254740993 || f.Items[0].Kind != "expense" || f.Items[0].Currency != "RUB" {
					t.Fatalf("operation facts: %+v", f)
				}
			case "analytics":
				if got.Analytics == nil || got.Analytics.HasData {
					t.Fatalf("no-data facts: %+v", got.Analytics)
				}
			}
			if (tt.name == "aggregate" || tt.name == "find") && !strings.Contains(string(got.JSON), `"9007199254740993"`) {
				t.Fatal("money lost exact integer representation")
			}
			if tt.name == "analytics" && !got.Evidence.NoData {
				t.Fatal("missing server no-data evidence")
			}
			if tt.name == "aggregate" && !got.Evidence.Truncated {
				t.Fatal("lost truncation evidence")
			}
			for i := 0; i < 2; i++ {
				x := <-p.seen
				if x.method != tt.name {
					t.Fatal(x.method)
				}
				if x.req.ProtoReflect().Get(x.req.ProtoReflect().Descriptor().Fields().ByName("user_id")).String() != subject.UserID {
					t.Fatal("wrong server subject")
				}
				for _, key := range []string{"authorization", "x-read-primary", "x-client-ip", "idempotency-key"} {
					if len(x.md.Get(key)) != 0 {
						t.Fatalf("forwarded %s", key)
					}
				}
				if token := x.md.Get("x-service-authorization"); len(token) != 1 || token[0] != "Bearer dev:assistant" {
					t.Fatal("missing service identity")
				}
				if id := x.md.Get("x-request-id"); len(id) != 1 || id[0] != "financial-request" {
					t.Fatal(id)
				}
				bound := 6 * time.Second
				if tt.name == "analytics" {
					bound = 11 * time.Second
				}
				if x.remaining <= 0 || x.remaining > bound {
					t.Fatalf("unbounded peer call: %s", x.remaining)
				}
				switch q := x.req.(type) {
				case *statementv1.AggregateOperationsRequest:
					if q.Filter.TransferMode != commonv1.TransferMode_TRANSFER_MODE_WITHOUT || q.Filter.GetSelection() != aggregateFilter.Selection || q.Filter.GetAmountToMinor() != maxAmount || !q.Filter.From.AsTime().Equal(from) || q.Limit != 1 || len(q.GroupBy) != 2 || q.GroupBy[0] != statementv1.GroupBy_GROUP_BY_MERCHANT || q.GroupBy[1] != statementv1.GroupBy_GROUP_BY_MONTH {
						t.Fatal("aggregate filters changed")
					}
				case *statementv1.FindOperationsRequest:
					if q.Filter.TransferMode != commonv1.TransferMode_TRANSFER_MODE_WITHOUT || q.Order != statementv1.ToolSortOrder_TOOL_SORT_ORDER_LARGEST || q.Limit != 1 || q.Filter.GetQ() != filter.Query {
						t.Fatal("find filters changed")
					}
				case *analyticsv1.GetAnalyticsRequest:
					if q.TransferMode != commonv1.TransferMode_TRANSFER_MODE_WITHOUT || q.Period != commonv1.PeriodType_PERIOD_TYPE_QUARTER || q.GetPeriodKey() != "2026-09" {
						t.Fatal("analytics parameters changed")
					}
				case *analyticsv1.ListPeriodsRequest:
					if q.Period != commonv1.PeriodType_PERIOD_TYPE_YEAR {
						t.Fatal("wrong period kind")
					}
				}
			}
		})
	}
}

func TestIntegrationFinancialDataRealFailureIsSanitized(t *testing.T) {
	p := &financialPeer{failure: status.Error(codes.PermissionDenied, "private-provider-error")}
	c := startFinancialPeer(t, p)
	_, err := c.Analytics(t.Context(), tooldata.Subject{UserID: "verified", TransferMode: "with"}, tooldata.AnalyticsArgs{})
	if !errors.Is(err, tooldata.ErrUnavailable) || strings.Contains(err.Error(), "private") {
		t.Fatalf("%v", err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.calls["analytics"] != 1 {
		t.Fatal("retried a non-retryable failure")
	}
}

func TestIntegrationFinancialDataRealCancellation(t *testing.T) {
	p := &financialPeer{wait: true}
	c := startFinancialPeer(t, p)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := c.Overview(ctx, tooldata.Subject{UserID: "verified", TransferMode: "with"})
		done <- err
	}()
	select {
	case <-p.seen:
	case <-time.After(3 * time.Second):
		t.Fatal("peer was not called")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancellation did not release caller")
	}
}

func TestIntegrationFinancialDataRealEarlierDeadline(t *testing.T) {
	p := &financialPeer{wait: true}
	c := startFinancialPeer(t, p)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	_, err := c.Periods(ctx, tooldata.Subject{UserID: "verified", TransferMode: "with"}, tooldata.PeriodsArgs{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}
