package client

import (
	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	statementv1 "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	"google.golang.org/protobuf/proto"

	"github.com/fin-assist/assistant/internal/datarevision"
	"github.com/fin-assist/assistant/internal/tooldata"
)

func financialRevisions(wire []*commonv1.DataRevision) (datarevision.Current, error) {
	if len(wire) == 0 {
		return nil, nil
	}
	result := make(datarevision.Current, len(wire))
	for _, v := range wire {
		if v == nil {
			return nil, tooldata.ErrInvalidResponse
		}
		if _, duplicate := result[v.Code]; duplicate {
			return nil, tooldata.ErrInvalidResponse
		}
		result[v.Code] = v.Value
	}
	if !datarevision.ValidCurrent(result) {
		return nil, tooldata.ErrInvalidResponse
	}
	return result, nil
}

// sourceRevisions reads the revisions that accompanied these exact facts. It
// never consults a later overview or current-version RPC to repair missing proof.
func sourceRevisions(message proto.Message) (datarevision.Current, error) {
	var wire []*commonv1.DataRevision
	switch r := message.(type) {
	case *statementv1.GetDataOverviewResponse:
		wire = r.GetVersion().GetRevisions()
	case *statementv1.AggregateOperationsResponse:
		wire = r.GetVersion().GetRevisions()
	case *statementv1.FindOperationsResponse:
		wire = r.GetVersion().GetRevisions()
	case *analyticsv1.GetAnalyticsResponse:
		wire = r.GetDataRevisions()
	case *analyticsv1.ListPeriodsResponse:
		wire = r.GetDataRevisions()
	}
	return financialRevisions(wire)
}

// modelFacts keeps private provenance out of model payloads. The original message
// and trusted Evidence retain their exact vectors, independently of JSON numbers.
func modelFacts(message proto.Message) proto.Message {
	result := proto.Clone(message)
	switch r := result.(type) {
	case *statementv1.GetDataOverviewResponse:
		if r.Version != nil {
			r.Version.Revisions = nil
		}
	case *statementv1.AggregateOperationsResponse:
		if r.Version != nil {
			r.Version.Revisions = nil
		}
	case *statementv1.FindOperationsResponse:
		if r.Version != nil {
			r.Version.Revisions = nil
		}
	case *analyticsv1.GetAnalyticsResponse:
		r.DataRevisions = nil
	case *analyticsv1.ListPeriodsResponse:
		r.DataRevisions = nil
	}
	return result
}
