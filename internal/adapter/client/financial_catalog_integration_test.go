package client

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/fin-assist/assistant/internal/tooldata"
)

func TestIntegrationFinancialCatalogValidatesReceivedProtobufBeforeTrustingFacts(t *testing.T) {
	for _, tc := range []struct {
		name, kind, data, status, lock string
		version                        int32
		valid                          bool
	}{
		{"ready-tile", "tile_group", `{"tiles":[{"id":"metric","status":"ready","value_kind":"amount","value":12345}]}`, "", "", 1, true},
		{"locked-tile", "tile_group", `{"tiles":[{"id":"metric","status":"locked","lock":{}}]}`, "", "blocks[0].data.tiles[0]", 1, true},
		{"locked-block", "rows", `{"rows":[]}`, "locked", "blocks[0]", 1, true},
		{"future-block", "future", `{"unknown":"compatible"}`, "ready", "", 7, true},
		{"future-tile-version", "tile_group", `{"tiles":[]}`, "", "", 2, false},
		{"missing-tiles", "tile_group", `{}`, "", "", 1, false},
		{"non-list-tiles", "tile_group", `{"tiles":"private upstream data"}`, "", "", 1, false},
		{"non-object-tile", "tile_group", `{"tiles":["private upstream data"]}`, "", "", 1, false},
		{"missing-id", "tile_group", `{"tiles":[{"title":"Показатель"}]}`, "", "", 1, false},
		{"numeric-id", "tile_group", `{"tiles":[{"id":17}]}`, "", "", 1, false},
		{"numeric-state", "tile_group", `{"tiles":[{"id":"metric","status":17}]}`, "", "", 1, false},
		{"unknown-state", "tile_group", `{"tiles":[{"id":"metric","status":"future"}]}`, "", "", 1, false},
		{"missing-lock", "tile_group", `{"tiles":[{"id":"metric","status":"locked"}]}`, "", "", 1, false},
		{"malformed-lock", "tile_group", `{"tiles":[{"id":"metric","status":"locked","lock":"private"}]}`, "", "", 1, false},
		{"numeric-kind", "rows", `{"rows":[{"label":"Сумма","value_kind":17,"value":12}]}`, "", "", 1, false},
		{"fractional-amount", "rows", `{"rows":[{"label":"Сумма","value_kind":"amount","value":12.5}]}`, "", "", 1, false},
		{"inexact-amount", "rows", `{"rows":[{"label":"Сумма","value_kind":"amount","value":9007199254740992}]}`, "", "", 1, false},
		{"unknown-value-kind", "rows", `{"rows":[{"label":"Сумма","value_kind":"future","value":12}]}`, "", "", 1, false},
		{"unknown-block-state", "rows", `{"rows":[]}`, "future", "", 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, fixture, subject := financialFixture(t)
			var fields map[string]any
			if err := json.Unmarshal([]byte(tc.data), &fields); err != nil {
				t.Fatal(err)
			}
			data, err := structpb.NewStruct(fields)
			if err != nil {
				t.Fatal(err)
			}
			block := &commonv1.Block{Id: "catalog", Type: tc.kind, V: tc.version, Status: tc.status, Data: data}
			if tc.status == "locked" {
				block.Lock = &commonv1.BlockLock{Text: "Показатель закрыт"}
			}
			fixture.analytics.Blocks = []*commonv1.Block{block}
			peer := &financialPeer{analyticsResult: fixture.analytics}
			client := startFinancialPeer(t, peer)
			result, err := client.Analytics(t.Context(), subject, tooldata.AnalyticsArgs{Period: "month", PeriodKey: "2026-09"})
			if !tc.valid {
				if !errors.Is(err, tooldata.ErrInvalidResponse) || len(result.JSON) != 0 || result.Analytics != nil || strings.Contains(err.Error(), "private") {
					t.Fatalf("malformed transport response became trusted facts: %+v err=%v", result, err)
				}
			} else if err != nil || result.Analytics == nil || len(result.Analytics.Blocks) != 1 || strings.Join(result.Evidence.LockedPaths, ",") != tc.lock {
				t.Fatalf("compatible facts or selected locks lost: %+v err=%v", result, err)
			}
			call := <-peer.seen
			if call.method != "analytics" || len(call.md.Get("authorization")) != 0 || len(call.md.Get("x-service-authorization")) != 1 || call.md.Get("x-service-authorization")[0] != "Bearer dev:assistant" {
				t.Fatalf("financial read crossed the identity boundary: %+v", call)
			}
		})
	}
}
