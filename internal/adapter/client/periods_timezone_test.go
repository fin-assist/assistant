package client

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/fin-assist/assistant/internal/datarevision"
	"github.com/fin-assist/assistant/internal/tooldata"
)

func TestUnitFinancialPeriodsOptionalEmptyTimezone(t *testing.T) {
	const sourceZone = "Europe/Moscow"
	const revision int64 = 7
	for _, test := range []struct {
		name    string
		empty   bool
		zone    string
		invalid bool
	}{
		{name: "empty without state", empty: true},
		{name: "empty with source zone", empty: true, zone: sourceZone},
		{name: "populated with source zone", zone: sourceZone},
		{name: "populated without zone", invalid: true},
		{name: "empty with unknown zone", empty: true, zone: "Unknown/Zone", invalid: true},
		{name: "empty with local zone", empty: true, zone: "Local", invalid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, peer, subject := financialFixture(t)
			if test.empty {
				peer.periods.Periods = nil
			}
			peer.periods.TimeZone = test.zone
			peer.periods.DataRevisions = strictWire(revision)
			got, err := c.Periods(t.Context(), subject, tooldata.PeriodsArgs{})
			if test.invalid {
				if !errors.Is(err, tooldata.ErrInvalidResponse) {
					t.Fatalf("invalid source zone accepted: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Evidence.NoData != test.empty || got.Evidence.Source == nil || got.Evidence.Source.TimeZone != test.zone {
				t.Fatalf("source facts changed: %+v", got.Evidence)
			}
			want := datarevision.Current{"categories": revision, "uploads": revision, "time_zone": revision}
			if !reflect.DeepEqual(got.Evidence.Revisions, want) || !reflect.DeepEqual(got.Evidence.Source.Version.Revisions, want) {
				t.Fatalf("private provenance lost: %+v", got.Evidence)
			}
			if strings.Contains(string(got.JSON), "revisions") || test.zone == "" && strings.Contains(string(got.JSON), "time_zone") {
				t.Fatalf("private or invented metadata in model copy: %s", got.JSON)
			}
		})
	}
}
