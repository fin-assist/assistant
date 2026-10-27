package client

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	statementv1 "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	"google.golang.org/protobuf/proto"

	"github.com/fin-assist/assistant/internal/datarevision"
	"github.com/fin-assist/assistant/internal/tooldata"
)

func strictWire(value int64) []*commonv1.DataRevision {
	return []*commonv1.DataRevision{{Code: "categories", Value: value}, {Code: "uploads", Value: value}, {Code: "time_zone", Value: value}}
}
func TestUnitFinancialSourcesCarryExactPrivateRevisions(t *testing.T) {
	c, f, subject := financialFixture(t)
	f.overview.Version.Revisions = strictWire(9223372036854775807)
	f.aggregate.Version.Revisions = strictWire(9223372036854775807)
	f.find.Version.Revisions = strictWire(9223372036854775807)
	f.analytics.DataRevisions = strictWire(4)
	f.periods.DataRevisions = strictWire(3)
	reads := []struct {
		want int64
		run  func() (tooldata.Result, error)
	}{{9223372036854775807, func() (tooldata.Result, error) { return c.Overview(t.Context(), subject) }}, {9223372036854775807, func() (tooldata.Result, error) { return c.Aggregate(t.Context(), subject, tooldata.AggregateArgs{}) }}, {9223372036854775807, func() (tooldata.Result, error) { return c.Find(t.Context(), subject, tooldata.FindArgs{}) }}, {4, func() (tooldata.Result, error) { return c.Analytics(t.Context(), subject, tooldata.AnalyticsArgs{}) }}, {3, func() (tooldata.Result, error) { return c.Periods(t.Context(), subject, tooldata.PeriodsArgs{}) }}}
	for _, r := range reads {
		got, err := r.run()
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got.Evidence.Revisions, datarevision.Current{"categories": r.want, "uploads": r.want, "time_zone": r.want}) {
			t.Fatal(got.Evidence)
		}
		if strings.Contains(string(got.JSON), "revisions") {
			t.Fatal("private revisions exposed to model", string(got.JSON))
		}
	}
	if f.analytics.DataRevisions[0].Value != 4 || f.overview.Version.Revisions[0].Value != 9223372036854775807 {
		t.Fatal("source vectors were changed")
	}
	// has_data=false still has the source state's private baseline, without inventing calculated_at.
	f.analytics = &analyticsv1.GetAnalyticsResponse{DataRevisions: strictWire(0)}
	got, err := c.Analytics(t.Context(), subject, tooldata.AnalyticsArgs{})
	if err != nil || got.Evidence.Source != nil || !got.Evidence.NoData || got.Evidence.Revisions["uploads"] != 0 || len(got.Evidence.Revisions) != 3 {
		t.Fatal(got, err)
	}
}
func TestUnitFinancialRejectsInvalidRevisionVectors(t *testing.T) {
	for _, bad := range [][]*commonv1.DataRevision{{nil}, {{Code: "uploads", Value: -1}}, {{Code: "UPPER", Value: 1}}, {{Code: "PRIVATE invalid code", Value: 1}}, {{Code: "uploads", Value: 0}, {Code: "uploads", Value: 1}}} {
		c, f, subject := financialFixture(t)
		f.overview.Version = &statementv1.DataVersion{Revisions: bad}
		if _, err := c.Overview(t.Context(), subject); err == nil || strings.Contains(err.Error(), "PRIVATE") {
			t.Fatal(err)
		}
		version := &versionClient{response: &statementv1.GetDataVersionResponse{Version: f.overview.Version}}
		if _, err := NewDataVersions(version).Get(t.Context(), "user"); err == nil || strings.Contains(err.Error(), "PRIVATE") {
			t.Fatal(err)
		}
	}
	c, f, subject := financialFixture(t)
	f.analytics.DataRevisions = []*commonv1.DataRevision{{Code: "future", Value: 10}}
	original := proto.Clone(f.analytics)
	r, err := c.Analytics(t.Context(), subject, tooldata.AnalyticsArgs{})
	if err != nil || r.Evidence.Revisions["future"] != 10 {
		t.Fatal(r, err)
	}
	if !proto.Equal(original, f.analytics) {
		t.Fatal("source changed")
	}
}

func TestUnitRevisionWireBoundaries(t *testing.T) {
	wire := make([]*commonv1.DataRevision, 32)
	for i := range wire {
		wire[i] = &commonv1.DataRevision{Code: fmt.Sprintf("kind_%d", i), Value: int64(i)}
	}
	parsed, err := financialRevisions(wire)
	if err != nil || len(parsed) != 32 {
		t.Fatal(parsed, err)
	}
	wire = append(wire, &commonv1.DataRevision{Code: "one_too_many", Value: 0})
	if _, err := financialRevisions(wire); !errors.Is(err, tooldata.ErrInvalidResponse) {
		t.Fatal(err)
	}
	for _, code := range []string{strings.Repeat("x", 65), "", "has-hyphen"} {
		if _, err := financialRevisions([]*commonv1.DataRevision{{Code: code}}); !errors.Is(err, tooldata.ErrInvalidResponse) {
			t.Fatal(err)
		}
	}
	if _, err := financialRevisions([]*commonv1.DataRevision{{Code: strings.Repeat("x", 64)}}); err != nil {
		t.Fatal(err)
	}
}
