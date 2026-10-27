package app_test

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	assistantv1 "github.com/fin-assist/contracts/gen/go/finassist/assistant/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	statementv1 "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/fin-assist/assistant/internal/datarevision"
)

// This exercises actual app wiring, gRPC neighbors, financial adapters, model
// continuation, generation finish, durable PG provenance and enriched history.
func strictGenerationProvenance(t *testing.T) {
	for _, variant := range []string{"equal", "find-only", "mixed-stale", "legacy-periods", "no-data", "pure-no-data", "anonymous-heading-realwire", "invalid-analytics"} {
		t.Run(variant, func(t *testing.T) {
			p := presenterMonthlyPeers()
			p.currentRevisions = strictAPIWire(5, 5, 5)
			p.overviewResponse.Version.Revisions = strictAPIWire(5, 5, 5)
			p.aggregateResponse.Version.Revisions = strictAPIWire(5, 5, 5)
			p.findResponse = &statementv1.FindOperationsResponse{Bounds: generationAPIBounds(), Range: generationAPIRange(), TimeZone: "Europe/Moscow", Version: &statementv1.DataVersion{Revisions: strictAPIWire(5, 5, 5)}}
			p.analyticsResponse = presenterAnalytics(t)
			p.analyticsResponse.DataRevisions = strictAPIWire(5, 5, 5)
			p.periodsResponse = &analyticsv1.ListPeriodsResponse{TimeZone: "Europe/Moscow", DataRevisions: strictAPIWire(5, 5, 5)}
			var want []assistantv1.ChangedSince
			switch variant {
			case "find-only":
				p.findResponse.Version.Revisions = strictAPIWire(5, 2, 5)
				want = []assistantv1.ChangedSince{assistantv1.ChangedSince_CHANGED_SINCE_UPLOADS}
			case "mixed-stale":
				p.analyticsResponse.State.Stale = true
				p.analyticsResponse.DataRevisions = strictAPIWire(2, 2, 2)
				p.periodsResponse.DataRevisions = strictAPIWire(3, 3, 3)
				want = []assistantv1.ChangedSince{assistantv1.ChangedSince_CHANGED_SINCE_CATEGORIES, assistantv1.ChangedSince_CHANGED_SINCE_UPLOADS, assistantv1.ChangedSince_CHANGED_SINCE_TIME_ZONE}
			case "legacy-periods":
				p.periodsResponse.DataRevisions = nil
				want = []assistantv1.ChangedSince{assistantv1.ChangedSince_CHANGED_SINCE_UNVERIFIED}
			case "no-data", "pure-no-data":
				p.analyticsResponse = &analyticsv1.GetAnalyticsResponse{DataRevisions: strictAPIWire(2, 2, 2)}
				want = []assistantv1.ChangedSince{assistantv1.ChangedSince_CHANGED_SINCE_CATEGORIES, assistantv1.ChangedSince_CHANGED_SINCE_UPLOADS, assistantv1.ChangedSince_CHANGED_SINCE_TIME_ZONE}
			case "anonymous-heading-realwire":
				raw, err := os.ReadFile("../adapter/client/testdata/analytics-month-anonymous-heading.json")
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
				wire.DataRevisions = strictAPIWire(5, 5, 5)
				p.analyticsResponse = wire
			case "invalid-analytics":
				p.analyticsResponse.DataRevisions = []*commonv1.DataRevision{{Code: "PRIVATE", Value: 1}, {Code: "PRIVATE", Value: 2}}
			}
			m := generationAPIModel(t, func(n int, w http.ResponseWriter, _ *http.Request) {
				calls := []string{"get_analytics", "list_periods", "aggregate_operations", "find_operations", "get_data_overview"}
				if variant == "pure-no-data" {
					calls = calls[:2]
				}
				if n <= len(calls) {
					args := `{}`
					if n == 3 {
						args = `{"all_time":true,"group_by":["month"]}`
					}
					if n == 4 {
						args = `{"all_time":true}`
					}
					_, _ = io.WriteString(w, generationAPIFunctionID(calls[n-1], calls[n-1], args))
					return
				}
				_, _ = io.WriteString(w, generationAPIText("Проверенный финансовый ответ"))
			})
			env := generationAPIEnv(t, p, m)
			accepted := generationAPIAsk(t, env, generationAPIUser, generationAPIKey(), "Сравни финансовые данные", commonv1.TransferMode_TRANSFER_MODE_WITHOUT)
			if variant == "invalid-analytics" {
				presenterAssertFailure(t, env, generationAPIUser, accepted.AnswerId)
				return
			}
			done, failed := generationAPITerminal(t, env, generationAPIUser, accepted.AnswerId)
			if done == nil || failed != nil || done.Charged == (variant == "no-data" || variant == "pure-no-data") {
				t.Fatalf("terminal: %v %v model_steps=%d calls=%v", done, failed, m.count.Load(), p.calls)
			}
			saved := presenterHistory(t, env, generationAPIUser)
			if variant == "pure-no-data" {
				if saved.Source != nil {
					t.Fatalf("fabricated pure no-data source: %v", saved.Source)
				}
				var unknown bool
				if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT data_revisions IS NULL AND calculated_at IS NULL FROM answer WHERE id=$1 AND user_id=$2`, accepted.AnswerId, generationAPIUser).Scan(&unknown); err != nil || !unknown {
					t.Fatalf("pure no-data storage: %v %v", unknown, err)
				}
				stream, err := assistantv1.NewAssistantServiceClient(env.Conn).WatchAnswer(generationAPIContext(t, generationAPIUser, ""), &assistantv1.WatchAnswerRequest{AnswerId: accepted.AnswerId})
				if err != nil {
					t.Fatal(err)
				}
				first, err := stream.Recv()
				if err != nil || first.GetSnapshot() == nil || first.GetSnapshot().Source != nil {
					t.Fatalf("pure no-data snapshot: %v %v", first, err)
				}
				if p.count("version") != 0 {
					t.Fatal("version read fabricated a source")
				}
				return
			}

			if saved.Source == nil || !reflect.DeepEqual(saved.Source.ChangedSince, want) {
				t.Fatalf("history source: %v want %v", saved.Source, want)
			}
			stream, watchErr := assistantv1.NewAssistantServiceClient(env.Conn).WatchAnswer(generationAPIContext(t, generationAPIUser, ""), &assistantv1.WatchAnswerRequest{AnswerId: accepted.AnswerId})
			if watchErr != nil {
				t.Fatal(watchErr)
			}
			first, watchErr := stream.Recv()
			if watchErr != nil || !reflect.DeepEqual(first.GetSnapshot().GetSource().GetChangedSince(), want) {
				t.Fatalf("snapshot source: %v %v", first, watchErr)
			}
			var raw []byte
			if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT data_revisions FROM answer WHERE id=$1 AND user_id=$2`, accepted.AnswerId, generationAPIUser).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var baseline datarevision.Baseline
			if err := json.Unmarshal(raw, &baseline); err != nil {
				t.Fatal(err)
			}
			values := []int64{5}
			switch variant {
			case "mixed-stale":
				values = []int64{2, 3, 5}
			case "no-data":
				values = []int64{2, 5}
			}
			for _, code := range []string{"categories", "uploads", "time_zone"} {
				wantValues := values
				if variant == "find-only" && code == "uploads" {
					wantValues = []int64{2, 5}
				}
				if !reflect.DeepEqual(baseline.Values[code], wantValues) {
					t.Fatalf("lost %s provenance: %+v", code, baseline)
				}
			}
			if baseline.Incomplete != (variant == "legacy-periods") {
				t.Fatalf("incomplete: %+v", baseline)
			}
			for _, method := range []string{"overview", "aggregate", "find", "analytics", "periods"} {
				if p.count(method) != 1 {
					t.Fatalf("%s count %d", method, p.count(method))
				}
			}
			m.mu.Lock()
			defer m.mu.Unlock()
			for _, r := range m.requests {
				body, _ := json.Marshal(r.body)
				if strings.Contains(string(body), "data_revisions") || strings.Contains(string(body), `\"revisions\"`) {
					t.Fatal("private revisions leaked to model")
				}
			}
		})
	}
}
func TestAPIGenerationAllFinancialSourceRevisions(t *testing.T) { strictGenerationProvenance(t) }
