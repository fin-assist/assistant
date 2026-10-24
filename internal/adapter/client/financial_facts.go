package client

import (
	"encoding/json"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	statementv1 "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	"google.golang.org/protobuf/proto"

	"github.com/fin-assist/assistant/internal/tooldata"
)

// financialFacts copies already validated transport responses into application
// facts. The model-facing protobuf JSON remains a separate representation.
func financialFacts(result *tooldata.Result, message proto.Message) error {
	switch r := message.(type) {
	case *statementv1.GetDataOverviewResponse:
		facts := &tooldata.Overview{Zone: r.TimeZone}
		for _, month := range r.Months {
			bounds := tooldata.Bounds{Coverage: financialCoverage(month.Bounds.Coverage), DataFrom: financialTime(month.Bounds.DataFrom), DataTo: financialTime(month.Bounds.DataTo)}
			for _, gap := range month.Bounds.Gaps {
				bounds.Gaps = append(bounds.Gaps, *financialDomainRange(gap))
			}
			facts.Months = append(facts.Months, tooldata.Month{Month: month.Month, Range: *financialDomainRange(month.Range), Bounds: bounds})
		}
		result.Overview = facts
	case *statementv1.AggregateOperationsResponse:
		facts := &tooldata.Aggregate{Amount: r.TotalAmountMinor, Count: r.TotalOperationCount, Range: financialDomainRange(r.Range), Zone: r.TimeZone, Truncated: r.Truncated}
		for _, group := range r.Groups {
			fact := tooldata.Group{Amount: group.AmountMinor, Count: group.OperationCount, Average: group.AverageMinor, Filters: financialNavigation(group.Filters)}
			for _, key := range group.Keys {
				normalized := tooldata.GroupKey{Month: key.GetMonth()}
				if key.Category != nil {
					normalized.Category = &tooldata.CategoryKey{ID: key.Category.Id, Name: key.Category.Name}
				}
				if key.Merchant != nil {
					normalized.Merchant = &tooldata.MerchantKey{Title: key.Merchant.Title, Selection: key.Merchant.Selection}
				}
				fact.Keys = append(fact.Keys, normalized)
			}
			facts.Groups = append(facts.Groups, fact)
		}
		result.Aggregate = facts
	case *statementv1.FindOperationsResponse:
		facts := &tooldata.Operations{Count: int64(r.TotalCount), Range: financialDomainRange(r.Range), Zone: r.TimeZone}
		for _, item := range r.Items {
			kind := ""
			switch item.Kind {
			case statementv1.OperationKind_OPERATION_KIND_EXPENSE:
				kind = "expense"
			case statementv1.OperationKind_OPERATION_KIND_INCOME:
				kind = "income"
			case statementv1.OperationKind_OPERATION_KIND_OWN_TRANSFER:
				kind = "own_transfer"
			}
			facts.Items = append(facts.Items, tooldata.Operation{Title: item.Title, Kind: kind, Currency: item.Currency, Amount: item.AmountMinor, At: item.OccurredAt.AsTime()})
		}
		result.Operations = facts
	case *analyticsv1.GetAnalyticsResponse:
		facts := &tooldata.Analytics{HasData: r.HasData, Currency: r.Currency}
		if r.Params != nil {
			facts.Params = tooldata.AnalyticsParams{Period: financialPeriodName(r.Params.Period), Key: r.Params.PeriodKey, Mode: financialMode(r.Params.TransferMode)}
		}
		for _, block := range r.Blocks {
			normalized := tooldata.CatalogBlock{ID: block.Id, Type: block.Type, V: block.V, Status: block.Status, Title: block.GetTitle(), Alt: block.GetAltText(), Empty: block.GetEmptyText()}
			if block.Lock != nil {
				normalized.Lock = &struct {
					Text string `json:"text"`
				}{Text: block.Lock.Text}
			}
			if block.Data != nil {
				data, err := json.Marshal(block.Data)
				if err != nil {
					return tooldata.ErrInvalidResponse
				}
				normalized.Data = data
			}
			if block.Action.GetType() == "open_operations" {
				filter := financialNavigation(block.Action.Filter)
				if financialNavigationValid(filter) {
					normalized.Action = filter
				}
			}
			facts.Blocks = append(facts.Blocks, normalized)
		}
		result.Analytics = facts
	}
	return nil
}

func financialCoverage(coverage commonv1.Coverage) string {
	switch coverage {
	case commonv1.Coverage_COVERAGE_COMPLETE:
		return "complete"
	case commonv1.Coverage_COVERAGE_PARTIAL:
		return "partial"
	case commonv1.Coverage_COVERAGE_NO_DATA:
		return "no_data"
	}
	return ""
}
func financialMode(mode commonv1.TransferMode) string {
	switch mode {
	case commonv1.TransferMode_TRANSFER_MODE_WITH:
		return "with"
	case commonv1.TransferMode_TRANSFER_MODE_WITHOUT:
		return "without"
	}
	return ""
}
func financialNavigation(filter *commonv1.OperationsFilter) *tooldata.NavigationFilter {
	if filter == nil {
		return nil
	}
	kind := ""
	switch filter.Kind {
	case commonv1.KindFilter_KIND_FILTER_EXPENSE:
		kind = "expense"
	case commonv1.KindFilter_KIND_FILTER_INCOME:
		kind = "income"
	}
	result := &tooldata.NavigationFilter{From: financialTime(filter.From), To: financialTime(filter.To), Q: filter.GetQ(), CategoryID: filter.GetCategoryId(), Kind: kind, Mode: financialMode(filter.TransferMode), Selection: filter.GetSelection(), SelectionName: filter.GetSelectionName()}
	if filter.AmountFromMinor != nil {
		v := *filter.AmountFromMinor
		result.AmountFrom = &v
	}
	if filter.AmountToMinor != nil {
		v := *filter.AmountToMinor
		result.AmountTo = &v
	}
	return result
}
func financialNavigationValid(filter *tooldata.NavigationFilter) bool {
	if filter == nil || filter.Mode == "" {
		return false
	}
	if filter.From != nil && !financialInputTime(*filter.From) || filter.To != nil && !financialInputTime(*filter.To) {
		return false
	}
	if filter.From != nil && filter.To != nil && !filter.From.Before(*filter.To) {
		return false
	}
	if filter.AmountFrom != nil && *filter.AmountFrom < 0 || filter.AmountTo != nil && *filter.AmountTo < 0 {
		return false
	}
	if filter.AmountFrom != nil && filter.AmountTo != nil && *filter.AmountFrom > *filter.AmountTo {
		return false
	}
	return filter.From != nil || filter.To != nil || filter.Q != "" || filter.CategoryID != "" || filter.Kind != "" || filter.AmountFrom != nil || filter.AmountTo != nil || filter.Selection != ""
}
