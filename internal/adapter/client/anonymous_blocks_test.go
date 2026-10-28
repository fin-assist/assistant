package client

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/fin-assist/assistant/internal/datarevision"
	"github.com/fin-assist/assistant/internal/tooldata"
)

// This unmodified wire shape comes from actual BSD/analytics local API assembly,
// not a reduced block invented to satisfy the consumer's validation assumptions.
func anonymousAnalyticsWire(t *testing.T) *analyticsv1.GetAnalyticsResponse {
	t.Helper()
	raw, err := os.ReadFile("testdata/analytics-month-anonymous-heading.json")
	if err != nil {
		t.Fatal(err)
	}
	wire := &analyticsv1.GetAnalyticsResponse{}
	if err = protojson.Unmarshal(raw, wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.Blocks) != 9 || wire.Blocks[3].Type != "heading" || wire.Blocks[3].Id != "" {
		t.Fatal("real anonymous heading fixture changed")
	}
	return wire
}

func TestUnitFinancialAnalyticsAcceptsOptionalCatalogBlockIDs(t *testing.T) {
	for _, clearIDs := range []bool{false, true} {
		c, peer, subject := financialFixture(t)
		wire := anonymousAnalyticsWire(t)
		if clearIDs {
			for _, block := range wire.Blocks {
				block.Id = ""
			}
		}
		original := proto.Clone(wire)
		peer.analytics = wire
		result, err := c.Analytics(t.Context(), subject, tooldata.AnalyticsArgs{Period: "month", PeriodKey: "2026-09"})
		if err != nil {
			t.Fatal("legal anonymous catalog rejected", err)
		}
		if len(result.Analytics.Blocks) != 9 || result.Analytics.Blocks[3].ID != "" || result.Analytics.Blocks[3].Type != "heading" {
			t.Fatal("heading dropped or ID invented", result.Analytics)
		}
		if !reflect.DeepEqual(result.Evidence.Revisions, datarevision.Current{"categories": 0, "uploads": 1, "time_zone": 0}) || len(result.Evidence.LockedPaths) != 3 {
			t.Fatal("source/locks lost", result.Evidence)
		}
		if strings.Contains(string(result.JSON), "data_revisions") || !proto.Equal(wire, original) {
			t.Fatal("private vector leaked or original reply mutated")
		}
	}
	// Optional top-level IDs do not weaken primitive/state/lock/number validation.
	for _, mutate := range []func(*analyticsv1.GetAnalyticsResponse){
		func(r *analyticsv1.GetAnalyticsResponse) { r.Blocks[3].Type = "" },
		func(r *analyticsv1.GetAnalyticsResponse) { r.Blocks[3].V = 0 },
		func(r *analyticsv1.GetAnalyticsResponse) { r.Blocks[3].Status = "future" },
		func(r *analyticsv1.GetAnalyticsResponse) { r.Blocks[3].Status = "locked" },
		func(r *analyticsv1.GetAnalyticsResponse) {
			r.Blocks[3].Data = financialStruct(t, map[string]any{"value_kind": "amount", "value": 1.5})
		},
	} {
		c, peer, subject := financialFixture(t)
		peer.analytics = anonymousAnalyticsWire(t)
		mutate(peer.analytics)
		if _, err := c.Analytics(t.Context(), subject, tooldata.AnalyticsArgs{}); !errors.Is(err, tooldata.ErrInvalidResponse) {
			t.Fatal("anonymous block bypassed financial guards", err)
		}
	}
}

func TestIntegrationFinancialAnonymousHeadingOverGRPC(t *testing.T) {
	peer := &financialPeer{analyticsResult: anonymousAnalyticsWire(t)}
	c := startFinancialPeer(t, peer)
	subject := tooldata.Subject{UserID: "verified-subject", TransferMode: "without"}
	result, err := c.Analytics(t.Context(), subject, tooldata.AnalyticsArgs{Period: "month", PeriodKey: "2026-09"})
	if err != nil || len(result.Analytics.Blocks) != 9 || result.Analytics.Blocks[3].ID != "" || result.Analytics.Blocks[3].Type != "heading" {
		t.Fatalf("actual wire transport: %+v %v", result, err)
	}
	seen := <-peer.seen
	request := seen.req.(*analyticsv1.GetAnalyticsRequest)
	if seen.method != "analytics" || request.GetUserId() != subject.UserID || len(seen.md.Get("authorization")) != 0 || len(seen.md.Get("x-service-authorization")) != 1 || seen.md.Get("x-service-authorization")[0] != "Bearer dev:assistant" {
		t.Fatal("transport subject/metadata changed", seen)
	}
	if !reflect.DeepEqual(result.Evidence.Revisions, datarevision.Current{"categories": 0, "uploads": 1, "time_zone": 0}) || strings.Contains(string(result.JSON), "data_revisions") {
		t.Fatal("private source provenance was not preserved separately")
	}
}
