package app_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	assistantv1 "github.com/fin-assist/contracts/gen/go/finassist/assistant/v1"
	statementv1 "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fin-assist/assistant/internal/app/apitest"
	"github.com/fin-assist/assistant/internal/config"
)

// The schema deliberately leaves the evolving block/chip bodies to the application.
// Invalid durable data must fail the whole read, including after a reconnect.
func TestAPIDialogueRejectsInvalidDurableSnapshots(t *testing.T) {
	for _, tc := range []struct{ name, query string }{
		{"chips-object", `UPDATE answer SET chips='{}' WHERE id=$1`},
		{"chip-empty-label", `UPDATE answer SET chips='[{"screen":"operations"}]' WHERE id=$1`},
		{"chip-missing-filters", `UPDATE answer SET chips='[{"label":"private marker","screen":"operations"}]' WHERE id=$1`},
		{"chip-empty-filters", `UPDATE answer SET chips='[{"label":"private marker","screen":"operations","filters":{"transfer_mode":"without"}}]' WHERE id=$1`},
		{"chip-unknown-mode", `UPDATE answer SET chips='[{"label":"private marker","screen":"operations","filters":{"q":"food","transfer_mode":"future"}}]' WHERE id=$1`},
		{"chip-reversed-range", `UPDATE answer SET chips='[{"label":"private marker","screen":"operations","filters":{"from":"2026-11-01T00:00:00Z","to":"2026-10-01T00:00:00Z","transfer_mode":"without"}}]' WHERE id=$1`},
		{"chip-negative-amount", `UPDATE answer SET chips='[{"label":"private marker","screen":"operations","filters":{"amount_from":-1,"transfer_mode":"without"}}]' WHERE id=$1`},
		{"chip-unknown-kind", `UPDATE answer SET chips='[{"label":"private marker","screen":"operations","filters":{"kind":"future","transfer_mode":"without"}}]' WHERE id=$1`},
		{"chip-missing-params", `UPDATE answer SET chips='[{"label":"private marker","screen":"analytics"}]' WHERE id=$1`},
		{"chip-unknown-period", `UPDATE answer SET chips='[{"label":"private marker","screen":"analytics","params":{"period":"future","date":"2026-10","transfer_mode":"without"}}]' WHERE id=$1`},
		{"chip-unknown-screen", `UPDATE answer SET chips='[{"label":"private marker","screen":"future"}]' WHERE id=$1`},
		{"source-missing-bound", `UPDATE answer SET calculated_at='2026-10-23T12:00:00Z',data_from='2026-10-01T00:00:00Z' WHERE id=$1`},
		{"source-reversed-range", `UPDATE answer SET calculated_at='2026-10-23T12:00:00Z',data_from='2026-11-01T00:00:00Z',data_to='2026-10-01T00:00:00Z' WHERE id=$1`},
		{"source-unrepresentable-time", `UPDATE answer SET calculated_at='10000-01-01T00:00:00Z' WHERE id=$1`},
		{"block-invalid-envelope", `UPDATE answer_block SET type='rows',body='{"alt_text":"private marker","data":true}' WHERE answer_id=$1`},
		{"block-invalid-chart-shape", `UPDATE answer_block SET type='chart',body='{"alt_text":"private marker","data":true}' WHERE answer_id=$1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, target := startDialogueVersionPeer(t)
			env := apitest.Start(t, func(c *config.Config) { c.Peers["bank-statement-data"] = target })
			seedAPIDialogue(t, env, "complete")
			dialogueAPIExec(t, env, tc.query, apiDialogueAnswer)
			client := assistantv1.NewAssistantServiceClient(env.Conn)
			ctx, cancel := apiDialogueContext(t, apiDialogueUser)
			defer cancel()
			page, err := client.ListMessages(ctx, &assistantv1.ListMessagesRequest{})
			if page != nil || status.Code(err) != codes.Internal || strings.Contains(err.Error(), "private marker") {
				t.Fatalf("invalid snapshot produced history: %v %v", page, err)
			}
			stream, err := client.WatchAnswer(ctx, &assistantv1.WatchAnswerRequest{AnswerId: apiDialogueAnswer})
			if err != nil {
				t.Fatal(err)
			}
			event, err := stream.Recv()
			if event != nil || status.Code(err) != codes.Internal || strings.Contains(err.Error(), "private marker") {
				t.Fatalf("invalid snapshot produced stream content: %v %v", event, err)
			}
		})
	}
}

// Each mutation commits before NOTIFY is delivered. The stream must reject a
// regressed saved state rather than emitting a misleading partial replacement.
func TestAPIDialogueWatchRejectsCommittedRegressions(t *testing.T) {
	for _, tc := range []struct{ name, setup, mutate string }{
		{"removed-block", "", `DELETE FROM answer_block WHERE answer_id=$1`},
		{"changed-block-type", "", `UPDATE answer_block SET type='rows' WHERE answer_id=$1`},
		{"regressed-text", "", `UPDATE answer_block SET markdown='different' WHERE answer_id=$1`},
		{"changed-nontext", `UPDATE answer_block SET type='future',body='{"alt_text":"original"}' WHERE answer_id=$1`, `UPDATE answer_block SET body='{"alt_text":"replacement"}' WHERE answer_id=$1`},
		{"removed-source", `UPDATE answer SET calculated_at='2026-10-23T12:00:00Z' WHERE id=$1`, `UPDATE answer SET calculated_at=NULL WHERE id=$1`},
		{"removed-chip", `UPDATE answer SET chips='[{"label":"Food","screen":"operations","filters":{"q":"food","transfer_mode":"without"}}]' WHERE id=$1`, `UPDATE answer SET chips='[]' WHERE id=$1`},
		{"changed-chip", `UPDATE answer SET chips='[{"label":"Food","screen":"operations","filters":{"q":"food","transfer_mode":"without"}}]' WHERE id=$1`, `UPDATE answer SET chips='[{"label":"Other","screen":"operations","filters":{"q":"other","transfer_mode":"without"}}]' WHERE id=$1`},
		{"unknown-status", "", `UPDATE answer SET status='future' WHERE id=$1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, target := startDialogueVersionPeer(t)
			env := apitest.Start(t, func(c *config.Config) { c.Peers["bank-statement-data"] = target })
			seedAPIDialogue(t, env, "generating")
			if tc.setup != "" {
				dialogueAPIExec(t, env, tc.setup, apiDialogueAnswer)
			}
			ctx, cancel := apiDialogueContext(t, apiDialogueUser)
			defer cancel()
			stream, err := assistantv1.NewAssistantServiceClient(env.Conn).WatchAnswer(ctx, &assistantv1.WatchAnswerRequest{AnswerId: apiDialogueAnswer})
			if err != nil {
				t.Fatal(err)
			}
			if first, err := stream.Recv(); err != nil || first.GetSnapshot() == nil {
				t.Fatalf("initial snapshot: %v %v", first, err)
			}
			if err := env.App.DB.Run(ctx, func(ctx context.Context) error {
				if _, err := env.App.DB.Writer(ctx).Exec(ctx, tc.mutate, apiDialogueAnswer); err != nil {
					return err
				}
				_, err := env.App.DB.Writer(ctx).Exec(ctx, `SELECT pg_notify('assistant_answers',$1)`, apiDialogueAnswer)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if event, err := stream.Recv(); event != nil || status.Code(err) != codes.Internal {
				t.Fatalf("regressed snapshot produced an update: %v %v", event, err)
			}
			var state string
			if err := env.App.DB.Writer(ctx).QueryRow(ctx, `SELECT status FROM answer WHERE id=$1`, apiDialogueAnswer).Scan(&state); err != nil {
				t.Fatal(err)
			}
			if want := map[bool]string{true: "future", false: "generating"}[tc.name == "unknown-status"]; state != want {
				t.Fatalf("observation changed durable generation state: %s", state)
			}
		})
	}
}

func TestAPIDialogueWatchRefreshesSourceAcrossCommittedTextUpdates(t *testing.T) {
	peer, target := startDialogueVersionPeer(t)
	env := apitest.Start(t, func(c *config.Config) { c.Peers["bank-statement-data"] = target })
	seedAPIDialogue(t, env, "generating")
	setAPIDialogueSource(t, env)
	ctx, cancel := apiDialogueContext(t, apiDialogueUser)
	defer cancel()
	stream, err := assistantv1.NewAssistantServiceClient(env.Conn).WatchAnswer(ctx, &assistantv1.WatchAnswerRequest{AnswerId: apiDialogueAnswer})
	if err != nil {
		t.Fatal(err)
	}
	first, err := stream.Recv()
	if err != nil || len(first.GetSnapshot().GetSource().GetChangedSince()) != 1 {
		t.Fatalf("initial source: %v %v", first, err)
	}
	// Each source-bearing durable update rechecks current proof. A bare child
	// deadline keeps committed text available, with explicit unverified source.
	peer.mu.Lock()
	peer.read = func(context.Context, *statementv1.GetDataVersionRequest) (*statementv1.GetDataVersionResponse, error) {
		return nil, status.Error(codes.DeadlineExceeded, "private deadline")
	}
	peer.mu.Unlock()
	if err := env.App.DB.Run(ctx, func(ctx context.Context) error {
		if _, err := env.App.DB.Writer(ctx).Exec(ctx, `UPDATE answer_block SET markdown='saved and committed' WHERE answer_id=$1`, apiDialogueAnswer); err != nil {
			return err
		}
		if _, err := env.App.DB.Writer(ctx).Exec(ctx, `UPDATE answer SET status='complete' WHERE id=$1`, apiDialogueAnswer); err != nil {
			return err
		}
		_, err := env.App.DB.Writer(ctx).Exec(ctx, `SELECT pg_notify('assistant_answers',$1)`, apiDialogueAnswer)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if chunk, err := stream.Recv(); err != nil || chunk.GetChunk().GetText() != " and committed" {
		t.Fatalf("text update: %v %v", chunk, err)
	}
	if source, err := stream.Recv(); err != nil || len(source.GetSource().GetChangedSince()) != 1 || source.GetSource().GetChangedSince()[0] != assistantv1.ChangedSince_CHANGED_SINCE_UNVERIFIED {
		t.Fatalf("updated proof: %v %v", source, err)
	}
	if done, err := stream.Recv(); err != nil || done.GetDone() == nil {
		t.Fatalf("terminal: %v %v", done, err)
	}
	if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("terminal EOF: %v", err)
	}
	peer.mu.Lock()
	defer peer.mu.Unlock()
	if peer.calls != 2 {
		t.Fatalf("source fetched %d times, want two fresh comparisons", peer.calls)
	}
}
