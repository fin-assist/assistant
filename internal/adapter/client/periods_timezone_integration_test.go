package client

import (
	"errors"
	"reflect"
	"testing"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"

	"github.com/fin-assist/assistant/internal/datarevision"
	"github.com/fin-assist/assistant/internal/tooldata"
)

func TestIntegrationFinancialPeriodsEmptyTimezoneOverGRPC(t *testing.T) {
	const revision int64 = 7
	for _, test := range []struct {
		name     string
		response *analyticsv1.ListPeriodsResponse
		invalid  bool
	}{
		{name: "empty without state", response: &analyticsv1.ListPeriodsResponse{}},
		{name: "empty with source zone", response: &analyticsv1.ListPeriodsResponse{TimeZone: "Europe/Moscow"}},
		{name: "explicit invalid zone", response: &analyticsv1.ListPeriodsResponse{TimeZone: "Local"}, invalid: true},
		{name: "populated without zone", response: &analyticsv1.ListPeriodsResponse{Periods: []*analyticsv1.PeriodItem{{Key: "2026-09", Range: financialTransportRange(), Coverage: commonv1.Coverage_COVERAGE_COMPLETE}}}, invalid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.response.DataRevisions = strictWire(revision)
			peer := &financialPeer{periodsResult: test.response}
			c := startFinancialPeer(t, peer)
			subject := tooldata.Subject{UserID: "019be405-3b20-7000-8000-000000000002", TransferMode: "without"}
			got, err := c.Periods(t.Context(), subject, tooldata.PeriodsArgs{})
			if test.invalid {
				if !errors.Is(err, tooldata.ErrInvalidResponse) {
					t.Fatalf("invalid peer result accepted: %v", err)
				}
				return
			}
			if err != nil || !got.Evidence.NoData || got.Evidence.Source == nil || got.Evidence.Source.TimeZone != test.response.TimeZone {
				t.Fatalf("empty peer result: %+v %v", got.Evidence, err)
			}
			want := datarevision.Current{"categories": revision, "uploads": revision, "time_zone": revision}
			if !reflect.DeepEqual(got.Evidence.Revisions, want) {
				t.Fatalf("provenance changed by transport: %+v", got.Evidence)
			}
		})
	}
}
