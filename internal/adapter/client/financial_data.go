package client

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	statementv1 "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/fin-assist/assistant/internal/tooldata"
)

// FinancialLimits bounds results independently of model and generation budgets.
type FinancialLimits struct{ ResultBytes, CollectionItems int }

// FinancialData adapts system RPCs to the tool engine's transport-free data port.
type FinancialData struct {
	statement statementv1.AssistantToolsServiceClient
	analytics analyticsv1.AnalyticsServiceClient
	limits    FinancialLimits
}

var _ tooldata.Data = (*FinancialData)(nil)

// NewFinancialData requires explicit positive quotas. Unconfigured peers fail on use.
func NewFinancialData(statement statementv1.AssistantToolsServiceClient, analytics analyticsv1.AnalyticsServiceClient, limits FinancialLimits) (*FinancialData, error) {
	if limits.ResultBytes <= 0 || limits.CollectionItems <= 0 {
		return nil, tooldata.ErrInvalidArguments
	}
	return &FinancialData{statement: statement, analytics: analytics, limits: limits}, nil
}

// Overview reads data coverage and source capabilities for the verified subject.
func (c *FinancialData) Overview(ctx context.Context, subject tooldata.Subject) (tooldata.Result, error) {
	if _, err := financialSubject(subject); err != nil {
		return tooldata.Result{}, err
	}
	if c.statement == nil {
		return tooldata.Result{}, tooldata.ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	r, err := c.statement.GetDataOverview(ctx, &statementv1.GetDataOverviewRequest{UserId: subject.UserID})
	if err != nil {
		return tooldata.Result{}, financialPeerError(ctx)
	}
	if err = c.validate(r); err != nil {
		return tooldata.Result{}, err
	}
	if r.FullMonths < 0 || !financialZone(r.TimeZone) || !financialTimestamp(r.CalculatedAt) {
		return tooldata.Result{}, tooldata.ErrInvalidResponse
	}
	source := &tooldata.Source{TimeZone: r.TimeZone, CalculatedAt: financialTime(r.CalculatedAt), Version: financialVersion(r.Version)}
	if r.DataFrom != nil || r.DataTo != nil {
		rangeValue := &commonv1.TimeRange{Start: r.DataFrom, End: r.DataTo}
		if !financialRange(rangeValue) {
			return tooldata.Result{}, tooldata.ErrInvalidResponse
		}
		source.Range = financialDomainRange(rangeValue)
	}
	if r.HasData && source.Range == nil {
		return tooldata.Result{}, tooldata.ErrInvalidResponse
	}
	for _, month := range r.Months {
		if month == nil || !financialCalendarKey("month", month.Month) || !financialRange(month.Range) || !financialBounds(month.Bounds) {
			return tooldata.Result{}, tooldata.ErrInvalidResponse
		}
	}
	return c.result(r, tooldata.Evidence{NoData: !r.HasData, Source: source})
}

// Aggregate returns bounded signed totals and groups in the question's transfer mode.
func (c *FinancialData) Aggregate(ctx context.Context, subject tooldata.Subject, args tooldata.AggregateArgs) (tooldata.Result, error) {
	mode, err := financialSubject(subject)
	if err != nil {
		return tooldata.Result{}, err
	}
	filter, err := financialFilter(args.Filter, args.AllTime, mode)
	if err != nil {
		return tooldata.Result{}, err
	}
	limit := args.Limit
	if limit == 0 {
		limit = 50
	}
	if limit < 1 || limit > 200 || len(args.GroupBy) > 2 {
		return tooldata.Result{}, tooldata.ErrInvalidArguments
	}
	groups := make([]statementv1.GroupBy, 0, len(args.GroupBy))
	seen := map[string]bool{}
	for _, group := range args.GroupBy {
		if seen[group] {
			return tooldata.Result{}, tooldata.ErrInvalidArguments
		}
		seen[group] = true
		switch group {
		case "month":
			groups = append(groups, statementv1.GroupBy_GROUP_BY_MONTH)
		case "category":
			groups = append(groups, statementv1.GroupBy_GROUP_BY_CATEGORY)
		case "merchant":
			groups = append(groups, statementv1.GroupBy_GROUP_BY_MERCHANT)
		default:
			return tooldata.Result{}, tooldata.ErrInvalidArguments
		}
	}
	if c.statement == nil {
		return tooldata.Result{}, tooldata.ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	r, err := c.statement.AggregateOperations(ctx, &statementv1.AggregateOperationsRequest{UserId: subject.UserID, Filter: filter, AllTime: args.AllTime, GroupBy: groups, Limit: int32(limit)})
	if err != nil {
		return tooldata.Result{}, financialPeerError(ctx)
	}
	if err = c.validate(r); err != nil {
		return tooldata.Result{}, err
	}
	if len(r.Groups) > limit || r.TotalOperationCount < 0 || !financialBounds(r.Bounds) || !financialZone(r.TimeZone) || !financialTimestamp(r.CalculatedAt) || (r.Range != nil && !financialRange(r.Range)) {
		return tooldata.Result{}, tooldata.ErrInvalidResponse
	}
	for _, group := range r.Groups {
		if group == nil || group.OperationCount < 0 || len(group.Keys) != len(groups) || group.Filters == nil || group.Filters.TransferMode != mode {
			return tooldata.Result{}, tooldata.ErrInvalidResponse
		}
		for i, key := range group.Keys {
			if !financialGroupKey(key, groups[i]) {
				return tooldata.Result{}, tooldata.ErrInvalidResponse
			}
		}
	}
	return c.result(r, tooldata.Evidence{NoData: r.Bounds.Coverage == commonv1.Coverage_COVERAGE_NO_DATA, Truncated: r.Truncated, Source: &tooldata.Source{Range: financialDomainRange(r.Range), CalculatedAt: financialTime(r.CalculatedAt), TimeZone: r.TimeZone, Version: financialVersion(r.Version)}})
}

// Find returns a bounded operation list; an empty list alone is not missing coverage.
func (c *FinancialData) Find(ctx context.Context, subject tooldata.Subject, args tooldata.FindArgs) (tooldata.Result, error) {
	mode, err := financialSubject(subject)
	if err != nil {
		return tooldata.Result{}, err
	}
	filter, err := financialFilter(args.Filter, args.AllTime, mode)
	if err != nil {
		return tooldata.Result{}, err
	}
	limit := args.Limit
	if limit == 0 {
		limit = 20
	}
	if limit < 1 || limit > 100 {
		return tooldata.Result{}, tooldata.ErrInvalidArguments
	}
	order := statementv1.ToolSortOrder_TOOL_SORT_ORDER_NEWEST
	switch args.Order {
	case "", "newest":
	case "largest":
		order = statementv1.ToolSortOrder_TOOL_SORT_ORDER_LARGEST
	default:
		return tooldata.Result{}, tooldata.ErrInvalidArguments
	}
	if c.statement == nil {
		return tooldata.Result{}, tooldata.ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	r, err := c.statement.FindOperations(ctx, &statementv1.FindOperationsRequest{UserId: subject.UserID, Filter: filter, AllTime: args.AllTime, Order: order, Limit: int32(limit)})
	if err != nil {
		return tooldata.Result{}, financialPeerError(ctx)
	}
	if err = c.validate(r); err != nil {
		return tooldata.Result{}, err
	}
	if len(r.Items) > limit || r.TotalCount < int32(len(r.Items)) || !financialBounds(r.Bounds) || !financialZone(r.TimeZone) || (r.Range != nil && !financialRange(r.Range)) {
		return tooldata.Result{}, tooldata.ErrInvalidResponse
	}
	for _, item := range r.Items {
		if item == nil || item.Id == "" || item.Kind == statementv1.OperationKind_OPERATION_KIND_UNSPECIFIED || item.Status == statementv1.OperationStatus_OPERATION_STATUS_UNSPECIFIED || item.AmountMinor < 0 || !financialCurrency(item.Currency) || !financialTimestamp(item.OccurredAt) {
			return tooldata.Result{}, tooldata.ErrInvalidResponse
		}
	}
	return c.result(r, tooldata.Evidence{NoData: r.Bounds.Coverage == commonv1.Coverage_COVERAGE_NO_DATA, Truncated: int64(r.TotalCount) > int64(len(r.Items)), Source: &tooldata.Source{Range: financialDomainRange(r.Range), TimeZone: r.TimeZone, Version: financialVersion(r.Version)}})
}

// Analytics returns current catalog blocks; deprecated metric trees are excluded.
func (c *FinancialData) Analytics(ctx context.Context, subject tooldata.Subject, args tooldata.AnalyticsArgs) (tooldata.Result, error) {
	mode, err := financialSubject(subject)
	if err != nil {
		return tooldata.Result{}, err
	}
	period, err := financialPeriod(args.Period)
	if err != nil {
		return tooldata.Result{}, err
	}
	if args.PeriodKey != "" && !financialCompatibleKey(period, args.PeriodKey) {
		return tooldata.Result{}, tooldata.ErrInvalidArguments
	}
	if c.analytics == nil {
		return tooldata.Result{}, tooldata.ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	r, err := c.analytics.GetAnalytics(ctx, &analyticsv1.GetAnalyticsRequest{UserId: &subject.UserID, TransferMode: mode, Period: period, PeriodKey: financialOptional(args.PeriodKey)})
	if err != nil {
		return tooldata.Result{}, financialPeerError(ctx)
	}
	if r == nil {
		return tooldata.Result{}, tooldata.ErrInvalidResponse
	}
	// Ignore retired fields before validation and encoding: they are not evidence
	// and may disappear independently of the block catalog.
	r = proto.Clone(r).(*analyticsv1.GetAnalyticsResponse)
	for _, name := range []protoreflect.Name{"tiles", "expense_categories", "income_categories", "monthly_chart", "insights"} {
		if field := r.ProtoReflect().Descriptor().Fields().ByName(name); field != nil {
			r.ProtoReflect().Clear(field)
		}
	}
	if err = c.validate(r); err != nil {
		return tooldata.Result{}, err
	}
	if !r.HasData {
		// No-data responses contain no other fields by contract.
		if r.Params != nil || r.Period != nil || r.Navigation != nil || r.State != nil || len(r.Blocks) != 0 || r.TimeZone != "" || r.Currency != "" {
			return tooldata.Result{}, tooldata.ErrInvalidResponse
		}
		return c.result(r, tooldata.Evidence{NoData: true})
	}
	if r.Params == nil || r.Params.Period != period || r.Params.TransferMode != mode || !financialCalendarKey(financialPeriodName(period), r.Params.PeriodKey) || r.Period == nil || !financialRange(r.Period.Range) || !financialBounds(r.Period.Bounds) || r.State == nil || !financialTimestamp(r.State.CalculatedAt) || !financialZone(r.TimeZone) || !financialCurrency(r.Currency) {
		return tooldata.Result{}, tooldata.ErrInvalidResponse
	}
	if !financialTimestamp(r.State.DataFrom) || !financialTimestamp(r.State.DataTo) || !r.State.DataFrom.AsTime().Before(r.State.DataTo.AsTime()) || r.State.FullMonths < 0 || r.State.UnreadLinesCount < 0 {
		return tooldata.Result{}, tooldata.ErrInvalidResponse
	}
	var locked []string
	for i, block := range r.Blocks {
		paths, err := financialBlock(block, fmt.Sprintf("blocks[%d]", i))
		if err != nil {
			return tooldata.Result{}, err
		}
		locked = append(locked, paths...)
	}
	return c.result(r, tooldata.Evidence{NoData: r.Period.Bounds.Coverage == commonv1.Coverage_COVERAGE_NO_DATA, LockedPaths: locked, Source: &tooldata.Source{Range: financialDomainRange(r.Period.Range), CalculatedAt: financialTime(r.State.CalculatedAt), TimeZone: r.TimeZone}})
}

// Periods returns available calendar periods, preserving the service's order.
func (c *FinancialData) Periods(ctx context.Context, subject tooldata.Subject, args tooldata.PeriodsArgs) (tooldata.Result, error) {
	if _, err := financialSubject(subject); err != nil {
		return tooldata.Result{}, err
	}
	period, err := financialPeriod(args.Period)
	if err != nil {
		return tooldata.Result{}, err
	}
	if c.analytics == nil {
		return tooldata.Result{}, tooldata.ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	r, err := c.analytics.ListPeriods(ctx, &analyticsv1.ListPeriodsRequest{UserId: &subject.UserID, Period: period})
	if err != nil {
		return tooldata.Result{}, financialPeerError(ctx)
	}
	if err = c.validate(r); err != nil {
		return tooldata.Result{}, err
	}
	if !financialZone(r.TimeZone) {
		return tooldata.Result{}, tooldata.ErrInvalidResponse
	}
	for _, item := range r.Periods {
		if item == nil || !financialCalendarKey(financialPeriodName(period), item.Key) || !financialRange(item.Range) || (item.Coverage != commonv1.Coverage_COVERAGE_COMPLETE && item.Coverage != commonv1.Coverage_COVERAGE_PARTIAL) {
			return tooldata.Result{}, tooldata.ErrInvalidResponse
		}
	}
	return c.result(r, tooldata.Evidence{NoData: len(r.Periods) == 0, Source: &tooldata.Source{TimeZone: r.TimeZone}})
}

func financialSubject(subject tooldata.Subject) (commonv1.TransferMode, error) {
	if strings.TrimSpace(subject.UserID) == "" || !utf8.ValidString(subject.UserID) {
		return 0, tooldata.ErrInvalidArguments
	}
	switch subject.TransferMode {
	case "with":
		return commonv1.TransferMode_TRANSFER_MODE_WITH, nil
	case "without":
		return commonv1.TransferMode_TRANSFER_MODE_WITHOUT, nil
	default:
		return 0, tooldata.ErrInvalidArguments
	}
}

func financialFilter(filter tooldata.Filter, allTime bool, mode commonv1.TransferMode) (*commonv1.OperationsFilter, error) {
	if (allTime && (filter.From != nil || filter.To != nil)) || (filter.From != nil && !financialInputTime(*filter.From)) || (filter.To != nil && !financialInputTime(*filter.To)) || (filter.From != nil && filter.To != nil && !filter.From.Before(*filter.To)) || (filter.AmountFrom != nil && *filter.AmountFrom < 0) || (filter.AmountTo != nil && *filter.AmountTo < 0) || (filter.AmountFrom != nil && filter.AmountTo != nil && *filter.AmountFrom > *filter.AmountTo) || !utf8.ValidString(filter.Query) || !utf8.ValidString(filter.CategoryID) || !utf8.ValidString(filter.Selection) {
		return nil, tooldata.ErrInvalidArguments
	}
	kind := commonv1.KindFilter_KIND_FILTER_UNSPECIFIED
	switch filter.Kind {
	case "":
	case "expense":
		kind = commonv1.KindFilter_KIND_FILTER_EXPENSE
	case "income":
		kind = commonv1.KindFilter_KIND_FILTER_INCOME
	default:
		return nil, tooldata.ErrInvalidArguments
	}
	r := &commonv1.OperationsFilter{Q: financialOptional(filter.Query), CategoryId: financialOptional(filter.CategoryID), Selection: financialOptional(filter.Selection), Kind: kind, TransferMode: mode, AmountFromMinor: filter.AmountFrom, AmountToMinor: filter.AmountTo}
	if filter.From != nil {
		r.From = timestamppb.New(*filter.From)
	}
	if filter.To != nil {
		r.To = timestamppb.New(*filter.To)
	}
	return r, nil
}

func financialOptional(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
func financialPeriod(period string) (commonv1.PeriodType, error) {
	switch period {
	case "", "month":
		return commonv1.PeriodType_PERIOD_TYPE_MONTH, nil
	case "quarter":
		return commonv1.PeriodType_PERIOD_TYPE_QUARTER, nil
	case "year":
		return commonv1.PeriodType_PERIOD_TYPE_YEAR, nil
	default:
		return 0, tooldata.ErrInvalidArguments
	}
}
func financialPeriodName(period commonv1.PeriodType) string {
	switch period {
	case commonv1.PeriodType_PERIOD_TYPE_QUARTER:
		return "quarter"
	case commonv1.PeriodType_PERIOD_TYPE_YEAR:
		return "year"
	default:
		return "month"
	}
}
func financialCalendarKey(period, key string) bool {
	switch period {
	case "month":
		t, err := time.Parse("2006-01", key)
		return err == nil && t.Year() > 0 && t.Format("2006-01") == key
	case "quarter":
		return len(key) == 7 && key[4:6] == "-Q" && key[6] >= '1' && key[6] <= '4' && financialCalendarKey("year", key[:4])
	case "year":
		t, err := time.Parse("2006", key)
		return err == nil && t.Year() > 0 && t.Format("2006") == key
	}
	return false
}
func financialCompatibleKey(period commonv1.PeriodType, key string) bool {
	if financialCalendarKey(financialPeriodName(period), key) {
		return true
	}
	return (period == commonv1.PeriodType_PERIOD_TYPE_QUARTER && financialCalendarKey("month", key)) || (period == commonv1.PeriodType_PERIOD_TYPE_YEAR && (financialCalendarKey("month", key) || financialCalendarKey("quarter", key)))
}
func financialInputTime(at time.Time) bool {
	return !at.IsZero() && timestamppb.New(at).CheckValid() == nil
}
func financialTimestamp(at *timestamppb.Timestamp) bool {
	return at != nil && at.CheckValid() == nil && !at.AsTime().IsZero()
}
func financialTime(at *timestamppb.Timestamp) *time.Time {
	if at == nil {
		return nil
	}
	value := at.AsTime()
	return &value
}
func financialRange(r *commonv1.TimeRange) bool {
	return r != nil && financialTimestamp(r.Start) && financialTimestamp(r.End) && r.Start.AsTime().Before(r.End.AsTime())
}
func financialDomainRange(r *commonv1.TimeRange) *tooldata.Range {
	if r == nil {
		return nil
	}
	return &tooldata.Range{Start: r.Start.AsTime(), End: r.End.AsTime()}
}
func financialVersion(v *statementv1.DataVersion) tooldata.Version {
	return tooldata.Version{CategoriesChangedAt: financialTime(v.GetCategoriesChangedAt()), UploadsChangedAt: financialTime(v.GetUploadsChangedAt())}
}
func financialZone(zone string) bool {
	if zone == "" || zone == "Local" {
		return false
	}
	_, err := time.LoadLocation(zone)
	return err == nil
}
func financialCurrency(currency string) bool {
	return len(currency) == 3 && currency[0] >= 'A' && currency[0] <= 'Z' && currency[1] >= 'A' && currency[1] <= 'Z' && currency[2] >= 'A' && currency[2] <= 'Z'
}
func financialBounds(bounds *commonv1.DataBounds) bool {
	if bounds == nil {
		return false
	}
	switch bounds.Coverage {
	case commonv1.Coverage_COVERAGE_NO_DATA:
		return bounds.DataFrom == nil && bounds.DataTo == nil && len(bounds.Gaps) == 0
	case commonv1.Coverage_COVERAGE_COMPLETE, commonv1.Coverage_COVERAGE_PARTIAL:
		if !financialTimestamp(bounds.DataFrom) || !financialTimestamp(bounds.DataTo) || bounds.DataTo.AsTime().Before(bounds.DataFrom.AsTime()) {
			return false
		}
	default:
		return false
	}
	for _, gap := range bounds.Gaps {
		if !financialRange(gap) {
			return false
		}
	}
	return true
}
func financialGroupKey(key *statementv1.GroupKey, group statementv1.GroupBy) bool {
	if key == nil {
		return false
	}
	count := 0
	if key.Month != nil {
		count++
	}
	if key.Category != nil {
		count++
	}
	if key.Merchant != nil {
		count++
	}
	if count != 1 {
		return false
	}
	switch group {
	case statementv1.GroupBy_GROUP_BY_MONTH:
		return key.Month != nil && financialCalendarKey("month", *key.Month)
	case statementv1.GroupBy_GROUP_BY_CATEGORY:
		return key.Category != nil && key.Category.Id != ""
	case statementv1.GroupBy_GROUP_BY_MERCHANT:
		return key.Merchant != nil && key.Merchant.Selection != ""
	}
	return false
}
func financialPeerError(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return tooldata.ErrUnavailable
}

func (c *FinancialData) validate(message proto.Message) error {
	if message == nil || reflect.ValueOf(message).IsNil() {
		return tooldata.ErrInvalidResponse
	}
	return c.validateMessage(message.ProtoReflect())
}
func (c *FinancialData) validateMessage(message protoreflect.Message) error {
	if !message.IsValid() {
		return tooldata.ErrInvalidResponse
	}
	if at, ok := message.Interface().(*timestamppb.Timestamp); ok && !financialTimestamp(at) {
		return tooldata.ErrInvalidResponse
	}
	var invalid error
	message.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		if field.IsList() {
			list := value.List()
			if list.Len() > c.limits.CollectionItems {
				invalid = tooldata.ErrResultTooLarge
				return false
			}
			for i := 0; i < list.Len(); i++ {
				if invalid = c.validateValue(field, list.Get(i)); invalid != nil {
					return false
				}
			}
			return true
		}
		if field.IsMap() {
			mapping := value.Map()
			if mapping.Len() > c.limits.CollectionItems {
				invalid = tooldata.ErrResultTooLarge
				return false
			}
			mapping.Range(func(_ protoreflect.MapKey, v protoreflect.Value) bool {
				invalid = c.validateValue(field.MapValue(), v)
				return invalid == nil
			})
			return invalid == nil
		}
		invalid = c.validateValue(field, value)
		return invalid == nil
	})
	return invalid
}
func (c *FinancialData) validateValue(field protoreflect.FieldDescriptor, value protoreflect.Value) error {
	switch field.Kind() {
	case protoreflect.MessageKind:
		return c.validateMessage(value.Message())
	case protoreflect.EnumKind:
		if field.Enum().Values().ByNumber(value.Enum()) == nil {
			return tooldata.ErrInvalidResponse
		}
	}
	return nil
}
func (c *FinancialData) result(message proto.Message, evidence tooldata.Evidence) (tooldata.Result, error) {
	raw, err := (protojson.MarshalOptions{UseProtoNames: true}).Marshal(message)
	if err != nil {
		return tooldata.Result{}, tooldata.ErrInvalidResponse
	}
	if len(raw) > c.limits.ResultBytes {
		return tooldata.Result{}, tooldata.ErrResultTooLarge
	}
	return tooldata.Result{JSON: raw, Evidence: evidence}, nil
}
