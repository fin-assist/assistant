package app_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	assistantv1 "github.com/fin-assist/contracts/gen/go/finassist/assistant/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	statementv1 "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/fin-assist/assistant/internal/adapter/postgres"
	"github.com/fin-assist/assistant/internal/app/apitest"
	"github.com/fin-assist/assistant/internal/config"
	"github.com/fin-assist/assistant/internal/datarevision"
	"github.com/fin-assist/assistant/internal/dialogue"
	"github.com/fin-assist/assistant/internal/domain"
	"github.com/fin-assist/assistant/internal/generation"
)

func strictAPIWire(categories, uploads, zone int64) []*commonv1.DataRevision {
	return []*commonv1.DataRevision{{Code: "categories", Value: categories}, {Code: "uploads", Value: uploads}, {Code: "time_zone", Value: zone}}
}

// The version peer reads a real primary relation; held writes and a long-running
// RR reader reproduce the commit-order hole of changed_at > calculated_at.
func strictDelayedCommitHistory(t *testing.T) {
	peer, target := startDialogueVersionPeer(t)
	env := apitest.Start(t, func(c *config.Config) { c.Peers["bank-statement-data"] = target })
	seedAPIDialogue(t, env, "generating")
	dialogueAPIExec(t, env, `CREATE TABLE strict_revision_fixture(user_id uuid PRIMARY KEY,categories bigint NOT NULL,uploads bigint NOT NULL,time_zone bigint NOT NULL,changed_at timestamptz NOT NULL)`)
	changedAt := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	calculatedAt := changedAt.Add(time.Hour)
	dialogueAPIExec(t, env, `INSERT INTO strict_revision_fixture VALUES($1,1,1,1,$2)`, apiDialogueUser, changedAt)
	writer, err := env.App.DB.Primary().Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Rollback(context.Background()) }()
	if _, err = writer.Exec(t.Context(), `UPDATE strict_revision_fixture SET uploads=2,changed_at=$2 WHERE user_id=$1`, apiDialogueUser, changedAt); err != nil {
		t.Fatal(err)
	}
	reader, err := env.App.DB.Primary().BeginTx(t.Context(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Rollback(context.Background()) }()
	var old int64
	if err = reader.QueryRow(t.Context(), `SELECT uploads FROM strict_revision_fixture WHERE user_id=$1`, apiDialogueUser).Scan(&old); err != nil || old != 1 {
		t.Fatal(old, err)
	}
	if err = writer.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	var retained int64
	if err = reader.QueryRow(t.Context(), `SELECT uploads FROM strict_revision_fixture WHERE user_id=$1`, apiDialogueUser).Scan(&retained); err != nil || retained != 1 {
		t.Fatal("RR facts were not stale", retained, err)
	}
	baseline := datarevision.Baseline{Values: map[string][]int64{"categories": {1}, "uploads": {retained}, "time_zone": {1}}}
	out := generation.Outcome{Charged: true, Blocks: []dialogue.Block{{Type: "text", Version: 1, Text: "saved"}}, Source: &dialogue.Source{CalculatedAt: calculatedAt, TransferMode: "without", Revisions: baseline}}
	if err = reader.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	err = env.App.DB.Run(t.Context(), func(ctx context.Context) error {
		result, e := postgres.NewGenerations(env.App.DB).Finish(ctx, domain.GenerationTask{UserID: apiDialogueUser, QuestionID: apiDialogueQuestion, AnswerID: apiDialogueAnswer}, generation.PrepareFinish(out, calculatedAt))
		if e == nil && !result.Applied {
			t.Error("source was not durably saved")
		}
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	peer.mu.Lock()
	peer.read = func(ctx context.Context, req *statementv1.GetDataVersionRequest) (*statementv1.GetDataVersionResponse, error) {
		var cat, uploads, zone int64
		var stamp time.Time
		if err := env.App.DB.Primary().QueryRow(ctx, `SELECT categories,uploads,time_zone,changed_at FROM strict_revision_fixture WHERE user_id=$1`, req.UserId).Scan(&cat, &uploads, &zone, &stamp); err != nil {
			return nil, err
		}
		return &statementv1.GetDataVersionResponse{Version: &statementv1.DataVersion{Revisions: strictAPIWire(cat, uploads, zone), CategoriesChangedAt: timestamppb.New(stamp), UploadsChangedAt: timestamppb.New(stamp)}}, nil
	}
	peer.mu.Unlock()
	client := assistantv1.NewAssistantServiceClient(env.Conn)
	check := func(conn assistantv1.AssistantServiceClient) {
		t.Helper()
		page, e := conn.ListMessages(apitest.User(t.Context(), apiDialogueUser), &assistantv1.ListMessagesRequest{})
		if e != nil {
			t.Fatal(e)
		}
		source := page.GetMessages()[1].GetSource()
		if !reflect.DeepEqual(source.GetChangedSince(), []assistantv1.ChangedSince{assistantv1.ChangedSince_CHANGED_SINCE_UPLOADS}) {
			t.Fatal("delayed commit hidden by wall clock", source)
		}
	}
	check(client)
	// The settings clock moves backwards again; a no-data import still advances its revision.
	dialogueAPIExec(t, env, `UPDATE strict_revision_fixture SET uploads=3,changed_at=$2 WHERE user_id=$1`, apiDialogueUser, changedAt.Add(-24*time.Hour))
	check(client)
	// Restart/read through another complete assembly must preserve the same baseline.
	restarted := assistantv1.NewAssistantServiceClient(dialogueReplica(t, env))
	check(restarted)
	stream, err := restarted.WatchAnswer(apitest.User(t.Context(), apiDialogueUser), &assistantv1.WatchAnswerRequest{AnswerId: apiDialogueAnswer})
	if err != nil {
		t.Fatal(err)
	}
	first, err := stream.Recv()
	if err != nil || !reflect.DeepEqual(first.GetSnapshot().GetSource().GetChangedSince(), []assistantv1.ChangedSince{assistantv1.ChangedSince_CHANGED_SINCE_UPLOADS}) {
		t.Fatal(first, err)
	}
}
func TestAPISourceDelayedCommitAndBackwardClock(t *testing.T)         { strictDelayedCommitHistory(t) }
func TestIntegrationSourceDelayedCommitAndBackwardClock(t *testing.T) { strictDelayedCommitHistory(t) }

func strictSourceCases(t *testing.T) {
	peer, target := startDialogueVersionPeer(t)
	env := apitest.Start(t, func(c *config.Config) { c.Peers["bank-statement-data"] = target })
	seedAPIDialogue(t, env, "complete")
	setAPIDialogueSource(t, env)
	client := assistantv1.NewAssistantServiceClient(env.Conn)
	cases := []struct {
		name     string
		baseline *datarevision.Baseline
		current  []*commonv1.DataRevision
		want     []assistantv1.ChangedSince
	}{
		{"legacy", nil, strictAPIWire(1, 1, 1), []assistantv1.ChangedSince{3}},
		{"known zero", &datarevision.Baseline{Values: map[string][]int64{"categories": {0}, "uploads": {0}, "time_zone": {0}}}, strictAPIWire(0, 0, 0), nil},
		{"equal ignores timestamps", &datarevision.Baseline{Values: map[string][]int64{"categories": {1}, "uploads": {1}, "time_zone": {1}}}, strictAPIWire(1, 1, 1), nil},
		{"time zone", &datarevision.Baseline{Values: map[string][]int64{"categories": {1}, "uploads": {1}, "time_zone": {1}}}, strictAPIWire(1, 1, 2), []assistantv1.ChangedSince{4}},
		{"mixed stale analytics", &datarevision.Baseline{Values: map[string][]int64{"categories": {1, 2}, "uploads": {3, 4}, "time_zone": {1}}}, strictAPIWire(2, 4, 1), []assistantv1.ChangedSince{1, 2}},
		{"sticky incomplete", &datarevision.Baseline{Values: map[string][]int64{"categories": {1}, "uploads": {1}, "time_zone": {1}}, Incomplete: true}, strictAPIWire(1, 1, 1), []assistantv1.ChangedSince{3}},
		{"missing current", &datarevision.Baseline{Values: map[string][]int64{"categories": {1}, "uploads": {1}, "time_zone": {1}}}, nil, []assistantv1.ChangedSince{3}},
		{"partial baseline", &datarevision.Baseline{Values: map[string][]int64{"categories": {1}}}, strictAPIWire(1, 1, 1), []assistantv1.ChangedSince{3}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var raw []byte
			var err error
			if tc.baseline != nil {
				raw, err = json.Marshal(tc.baseline)
				if err != nil {
					t.Fatal(err)
				}
			}
			dialogueAPIExec(t, env, `UPDATE answer SET data_revisions=$3 WHERE user_id=$1 AND id=$2`, apiDialogueUser, apiDialogueAnswer, raw)
			peer.mu.Lock()
			peer.revisions = tc.current
			peer.stamp = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
			peer.mu.Unlock()
			page, err := client.ListMessages(apitest.User(t.Context(), apiDialogueUser), &assistantv1.ListMessagesRequest{})
			if err != nil {
				t.Fatal(err)
			}
			source := page.Messages[1].GetSource()
			if !reflect.DeepEqual(source.ChangedSince, tc.want) {
				t.Fatal(source, tc.want)
			}
			stream, err := client.WatchAnswer(apitest.User(t.Context(), apiDialogueUser), &assistantv1.WatchAnswerRequest{AnswerId: apiDialogueAnswer})
			if err != nil {
				t.Fatal(err)
			}
			first, err := stream.Recv()
			if err != nil || !reflect.DeepEqual(first.GetSnapshot().GetSource().ChangedSince, tc.want) {
				t.Fatal(first, err)
			}
		})
	}
	// Corrupt private provenance is unknown, while financial facts remain intact.
	peer.mu.Lock()
	peer.revisions = strictAPIWire(0, 0, 0)
	peer.mu.Unlock()
	for _, bad := range []string{`{"values":{"PRIVATEbad":[1]}}`, `{"values":{"uploads":[-1]}}`, `{"values":{"uploads":[1,1]}}`, `{"values":{"uploads":[]}}`, `[]`, `{"values":{"categories":[null],"uploads":[null],"time_zone":[null]},"incomplete":false}`, `{"values":{"categories":[0],"uploads":[0],"time_zone":[0]},"incomplete":null}`, `{"VALUES":{"categories":[0],"uploads":[0],"time_zone":[0]},"incomplete":false}`} {
		dialogueAPIExec(t, env, `UPDATE answer SET data_revisions=$3 WHERE user_id=$1 AND id=$2`, apiDialogueUser, apiDialogueAnswer, bad)
		page, err := client.ListMessages(apitest.User(t.Context(), apiDialogueUser), &assistantv1.ListMessagesRequest{})
		if err != nil || !reflect.DeepEqual(page.GetMessages()[1].GetSource().GetChangedSince(), []assistantv1.ChangedSince{assistantv1.ChangedSince_CHANGED_SINCE_UNVERIFIED}) {
			t.Fatal(page, err)
		}
		stream, err := client.WatchAnswer(apitest.User(t.Context(), apiDialogueUser), &assistantv1.WatchAnswerRequest{AnswerId: apiDialogueAnswer})
		if err != nil {
			t.Fatal(err)
		}
		first, err := stream.Recv()
		if err != nil || !reflect.DeepEqual(first.GetSnapshot().GetSource().GetChangedSince(), []assistantv1.ChangedSince{assistantv1.ChangedSince_CHANGED_SINCE_UNVERIFIED}) {
			t.Fatal(first, err)
		}
	}
	// Malformed peer versions fail safely instead of becoming proof of no changes.
	for _, bad := range [][]*commonv1.DataRevision{{{Code: "uploads", Value: 1}, {Code: "uploads", Value: 2}}, {{Code: "PRIVATE bad", Value: 1}}, {{Code: "uploads", Value: -1}}} {
		peer.mu.Lock()
		peer.revisions = bad
		peer.mu.Unlock()
		_, err := client.ListMessages(apitest.User(t.Context(), apiDialogueUser), &assistantv1.ListMessagesRequest{})
		if status.Code(err) != codes.Internal || strings.Contains(err.Error(), "PRIVATE") {
			t.Fatal(err)
		}
	}
}
func TestAPISourceRevisionLegacyMixedAndUnverified(t *testing.T)         { strictSourceCases(t) }
func TestIntegrationSourceRevisionLegacyMixedAndUnverified(t *testing.T) { strictSourceCases(t) }
