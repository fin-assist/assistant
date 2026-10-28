package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	statementv1 "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	"github.com/fin-assist/contracts/gen/go/finerr"
	grpcclient "github.com/fin-assist/go-grpc/client"
	"github.com/fin-assist/go-migrate"
	tombstone "github.com/fin-assist/go-tombstone"
	"github.com/jackc/pgx/v5"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/fin-assist/assistant/internal/account"
	"github.com/fin-assist/assistant/internal/adapter/client"
	"github.com/fin-assist/assistant/internal/adapter/postgres"
	"github.com/fin-assist/assistant/internal/app"
	"github.com/fin-assist/assistant/internal/datarevision"
	"github.com/fin-assist/assistant/internal/dialogue"
	"github.com/fin-assist/assistant/internal/domain"
	"github.com/fin-assist/assistant/internal/generation"
	"github.com/fin-assist/assistant/internal/testdb"
)

// This fixture assembles components, not the application/public AssistantService:
// PG18 + transactions/repositories/tombstones/notifications, direct Engine and
// dialogue ports, and real gRPC transports only to financial/version neighbors.
type revisionComposition struct {
	db       *postgres.DB
	guards   *tombstone.Store
	task     domain.GenerationTask
	notifies *postgres.AnswerNotifications
	accounts *account.Service
}

func newRevisionComposition(t *testing.T) *revisionComposition {
	t.Helper()
	dsn := testdb.DSN(t)
	cfg := migrate.DefaultConfig(dsn)
	cfg.Logger = slog.New(slog.DiscardHandler)
	if err := migrate.Up(t.Context(), cfg, app.MigrationSources()); err != nil {
		t.Fatal(err)
	}
	db, err := postgres.Open(t.Context(), postgres.Options{PrimaryDSN: dsn, AppName: "revision-composition", MaxConns: 6})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	guards, err := tombstone.New(db, db.Primary(), func() time.Time { return generationAPINow })
	if err != nil {
		t.Fatal(err)
	}
	n, err := postgres.NewAnswerNotifications(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(n.Close)
	e := &revisionComposition{db: db, guards: guards, notifies: n}
	e.accounts = account.New(db, postgres.NewAccounts(db), guards, func() time.Time { return generationAPINow }, 5)
	if err = e.accounts.SetTimezone(t.Context(), generationAPIUser, "Europe/Moscow", 1); err != nil {
		t.Fatal(err)
	}
	questions := postgres.NewQuestions(db)
	if err = db.Run(t.Context(), func(ctx context.Context) error {
		if _, err := questions.Lock(ctx, generationAPIUser); err != nil {
			return err
		}
		if err := questions.Charge(ctx, generationAPIUser, generationAPINow, 1); err != nil {
			return err
		}
		var err error
		e.task, err = questions.Create(ctx, generationAPIUser, "Сравни финансовые данные", "without", generationAPINow)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return e
}

func revisionNeighbor(t *testing.T, target string, options ...grpc.DialOption) *grpc.ClientConn {
	t.Helper()
	conn, err := grpcclient.Dial(target, grpcclient.Options{Peer: "bank-statement-data", Tokens: grpcclient.DevToken("assistant"), DialOptions: options})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func (e *revisionComposition) reads(versions dialogue.Versions) *dialogue.Service {
	return dialogue.New(postgres.NewDialogue(e.db), versions, e.notifies, storedDialogueAllowance{e.accounts})
}
func (e *revisionComposition) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := e.db.Primary().Exec(t.Context(), sql, args...); err != nil {
		t.Fatal(err)
	}
}
func (e *revisionComposition) finish(t *testing.T, baseline datarevision.Baseline) {
	t.Helper()
	out := generation.Outcome{Charged: true, Blocks: []dialogue.Block{{Type: "text", Version: 1, Text: "saved"}}, Source: &dialogue.Source{CalculatedAt: generationAPINow, TransferMode: "without", Revisions: baseline}}
	if err := e.db.Run(t.Context(), func(ctx context.Context) error {
		result, err := postgres.NewGenerations(e.db).Finish(ctx, e.task, generation.PrepareFinish(out, generationAPINow))
		if err == nil && !result.Applied {
			t.Error("terminal not applied")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
func revisionBaseline(v int64) datarevision.Baseline {
	return datarevision.Baseline{Values: map[string][]int64{"categories": {v}, "uploads": {v}, "time_zone": {v}}}
}
func revisionHistory(t *testing.T, reads *dialogue.Service) *dialogue.Answer {
	t.Helper()
	page, err := reads.List(t.Context(), generationAPIUser, "", 20)
	if err != nil || len(page.Messages) != 2 || page.Messages[1].Answer == nil {
		t.Fatalf("history: %+v %v", page, err)
	}
	return page.Messages[1].Answer
}
func revisionWatch(t *testing.T, e *revisionComposition, reads *dialogue.Service) []dialogue.Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var events []dialogue.Event
	if err := reads.Watch(ctx, generationAPIUser, e.task.AnswerID, func(event dialogue.Event) error { events = append(events, event); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Kind != "snapshot" || events[1].Kind != "done" {
		t.Fatalf("events: %+v", events)
	}
	return events
}

type revisionModel struct {
	calls    []string
	requests [][]byte
	step     int
}

func (m *revisionModel) Stream(_ context.Context, request generation.ModelRequest, emit func(generation.ModelDelta) error, reserve func([]byte) error) (generation.ModelResult, error) {
	raw, err := json.Marshal(request)
	if err != nil {
		return generation.ModelResult{}, err
	}
	if err = reserve(raw); err != nil {
		return generation.ModelResult{}, err
	}
	m.requests = append(m.requests, raw)
	if m.step < len(m.calls) {
		name := m.calls[m.step]
		m.step++
		args := `{}`
		if name == "aggregate_operations" {
			args = `{"all_time":true,"group_by":["month"]}`
		}
		if name == "find_operations" {
			args = `{"all_time":true}`
		}
		wire, err := json.Marshal(map[string]string{"id": name, "type": "function_call", "call_id": name, "name": name, "arguments": args, "status": "completed"})
		return generation.ModelResult{Output: []generation.ModelItem{{ID: name, Type: "function_call", CallID: name, Name: name, Arguments: args, Raw: wire}}}, err
	}
	text := "Проверенный финансовый ответ"
	if err = emit(generation.ModelDelta{Text: text}); err != nil {
		return generation.ModelResult{}, err
	}
	wire, err := json.Marshal(map[string]any{"id": "message", "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]string{"type": "output_text", "text": text}}})
	return generation.ModelResult{Output: []generation.ModelItem{{ID: "message", Type: "message", Raw: wire}}}, err
}

func TestIntegrationRevisionEngineFinancialNeighborsAndDurableHistory(t *testing.T) {
	for _, variant := range []string{"equal", "find-only", "mixed-stale", "legacy-periods", "no-data", "pure-no-data", "invalid-analytics"} {
		t.Run(variant, func(t *testing.T) {
			e := newRevisionComposition(t)
			p := presenterMonthlyPeers()
			p.currentRevisions = strictAPIWire(5, 5, 5)
			p.overviewResponse.Version.Revisions = strictAPIWire(5, 5, 5)
			p.aggregateResponse.Version.Revisions = strictAPIWire(5, 5, 5)
			p.findResponse = &statementv1.FindOperationsResponse{Bounds: generationAPIBounds(), Range: generationAPIRange(), TimeZone: "Europe/Moscow", Version: &statementv1.DataVersion{Revisions: strictAPIWire(5, 5, 5)}}
			p.analyticsResponse = presenterAnalytics(t)
			p.analyticsResponse.DataRevisions = strictAPIWire(5, 5, 5)
			p.periodsResponse = &analyticsv1.ListPeriodsResponse{TimeZone: "Europe/Moscow", DataRevisions: strictAPIWire(5, 5, 5)}
			want := []string{}
			switch variant {
			case "find-only":
				p.findResponse.Version.Revisions = strictAPIWire(5, 2, 5)
				want = []string{"uploads"}
			case "mixed-stale":
				p.analyticsResponse.State.Stale = true
				p.analyticsResponse.DataRevisions = strictAPIWire(2, 2, 2)
				p.periodsResponse.DataRevisions = strictAPIWire(3, 3, 3)
				want = []string{"categories", "uploads", "time_zone"}
			case "legacy-periods":
				p.periodsResponse.DataRevisions = nil
				want = []string{"unverified"}
			case "no-data", "pure-no-data":
				p.analyticsResponse = &analyticsv1.GetAnalyticsResponse{DataRevisions: strictAPIWire(2, 2, 2)}
				want = []string{"categories", "uploads", "time_zone"}
			case "invalid-analytics":
				p.analyticsResponse.DataRevisions = []*commonv1.DataRevision{{Code: "PRIVATE", Value: 1}, {Code: "PRIVATE", Value: 2}}
			}
			opts := generationAPIPeerOptions(t, p)["bank-statement-data"]
			conn := revisionNeighbor(t, "passthrough:///financial-components", opts...)
			data, err := client.NewFinancialData(statementv1.NewAssistantToolsServiceClient(conn), analyticsv1.NewAnalyticsServiceClient(conn), client.FinancialLimits{ResultBytes: 32768, CollectionItems: 200})
			if err != nil {
				t.Fatal(err)
			}
			model := &revisionModel{calls: []string{"get_analytics", "list_periods", "aggregate_operations", "find_operations", "get_data_overview"}}
			if variant == "pure-no-data" {
				model.calls = model.calls[:2]
			}
			engine, err := generation.New(postgres.NewGenerations(e.db), e.db, e.guards, model, data, generation.EngineLimits{Budget: generation.BudgetLimits{InputTokensPerStep: 24000, InputTokensTotal: 48000, HistoryTokens: 16000, RequestBytesPerStep: 65536, RequestBytesTotal: 131072, MaxSteps: 8, MaxFunctions: 16}, MaxOutputTokens: 4096, ParallelFunctions: 4, MaxSearchCalls: 3}, func() time.Time { return generationAPINow })
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			if err = engine.Run(ctx, e.task); err != nil {
				t.Fatal(err)
			}
			reads := e.reads(client.NewDataVersions(statementv1.NewAssistantToolsServiceClient(conn)))
			saved := revisionHistory(t, reads)
			if variant == "invalid-analytics" {
				if saved.Status != "failed" || saved.ErrorCode != "MODEL_ERROR" || saved.Charged || saved.Retryable == nil || !*saved.Retryable || saved.Source != nil {
					t.Fatalf("invalid vector did not refund/fail: %+v", saved)
				}
				limit, err := e.accounts.GetLimit(ctx, generationAPIUser)
				if err != nil || limit.Remaining != 5 {
					t.Fatalf("refund: %+v %v", limit, err)
				}
				return
			}
			if saved.Status != "complete" || saved.Charged == (variant == "no-data" || variant == "pure-no-data") {
				t.Fatalf("terminal: %+v", saved)
			}
			events := revisionWatch(t, e, reads)
			if variant == "pure-no-data" {
				if saved.Source != nil || events[0].Answer.Source != nil || p.count("version") != 0 {
					t.Fatal("fabricated source")
				}
				return
			}
			if saved.Source == nil || !reflect.DeepEqual(saved.Source.ChangedSince, want) || !reflect.DeepEqual(events[0].Answer.Source.ChangedSince, want) {
				t.Fatalf("history/snapshot: %+v want%v", saved.Source, want)
			}
			values := []int64{5}
			if variant == "mixed-stale" {
				values = []int64{2, 3, 5}
			}
			if variant == "no-data" {
				values = []int64{2, 5}
			}
			for _, code := range []string{"categories", "uploads", "time_zone"} {
				expected := values
				if variant == "find-only" && code == "uploads" {
					expected = []int64{2, 5}
				}
				if !reflect.DeepEqual(saved.Source.Revisions.Values[code], expected) {
					t.Fatalf("durable %s baseline: %+v", code, saved.Source.Revisions)
				}
			}
			if saved.Source.Revisions.Incomplete != (variant == "legacy-periods") {
				t.Fatal(saved.Source.Revisions)
			}
			for _, method := range []string{"overview", "aggregate", "find", "analytics", "periods"} {
				if p.count(method) != 1 {
					t.Fatalf("%s called%d", method, p.count(method))
				}
			}
			for _, raw := range model.requests {
				if strings.Contains(string(raw), "data_revisions") || strings.Contains(string(raw), `\"revisions\"`) {
					t.Fatal("private baseline leaked to model")
				}
			}
		})
	}
}

func TestIntegrationRevisionDialogueStoredBaselineAndPeerAvailability(t *testing.T) {
	e := newRevisionComposition(t)
	e.finish(t, revisionBaseline(0))
	peer, target := startDialogueVersionPeer(t)
	conn := revisionNeighbor(t, target)
	reads := e.reads(client.NewDataVersions(statementv1.NewAssistantToolsServiceClient(conn)))
	peer.revisions = strictAPIWire(0, 0, 0)
	for _, tc := range []struct {
		name    string
		raw     any
		current []*commonv1.DataRevision
		want    []string
	}{
		{"known-zero", `{"values":{"categories":[0],"uploads":[0],"time_zone":[0]},"incomplete":false}`, strictAPIWire(0, 0, 0), []string{}},
		{"legacy", nil, strictAPIWire(0, 0, 0), []string{"unverified"}},
		{"null-number", `{"values":{"categories":[null],"uploads":[null],"time_zone":[null]},"incomplete":false}`, strictAPIWire(0, 0, 0), []string{"unverified"}},
		{"null-completeness", `{"values":{"categories":[0],"uploads":[0],"time_zone":[0]},"incomplete":null}`, strictAPIWire(0, 0, 0), []string{"unverified"}},
		{"mixed", `{"values":{"categories":[1,2],"uploads":[3,4],"time_zone":[1]},"incomplete":false}`, strictAPIWire(2, 4, 2), []string{"categories", "uploads", "time_zone"}},
		{"missing-current", `{"values":{"categories":[0],"uploads":[0],"time_zone":[0]},"incomplete":false}`, nil, []string{"unverified"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e.exec(t, `UPDATE answer SET data_revisions=$3 WHERE user_id=$1 AND id=$2`, generationAPIUser, e.task.AnswerID, tc.raw)
			peer.mu.Lock()
			peer.revisions = tc.current
			peer.mu.Unlock()
			if got := revisionHistory(t, reads).Source; !reflect.DeepEqual(got.ChangedSince, tc.want) {
				t.Fatalf("history: %+v want%v", got, tc.want)
			}
			if got := revisionWatch(t, e, reads)[0].Answer.Source; !reflect.DeepEqual(got.ChangedSince, tc.want) {
				t.Fatalf("snapshot: %+v want%v", got, tc.want)
			}
		})
	}
	base, _ := json.Marshal(revisionBaseline(1))
	e.exec(t, `UPDATE answer SET data_revisions=$3 WHERE user_id=$1 AND id=$2`, generationAPIUser, e.task.AnswerID, base)
	detailed, _ := status.New(codes.Unavailable, "PRIVATE").WithDetails(&errdetails.ErrorInfo{Reason: "TOKEN_REVOKED"})
	for _, tc := range []struct {
		name    string
		err     error
		degrade bool
	}{
		{"unavailable", status.Error(codes.Unavailable, "PRIVATE"), true}, {"child-deadline", status.Error(codes.DeadlineExceeded, "PRIVATE"), true},
		{"token", finerr.InvalidToken(), false}, {"revoked", finerr.TokenRevoked(), false}, {"permission", status.Error(codes.PermissionDenied, "PRIVATE"), false}, {"detailed", detailed.Err(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			peer.mu.Lock()
			peer.read = func(context.Context, *statementv1.GetDataVersionRequest) (*statementv1.GetDataVersionResponse, error) {
				return nil, tc.err
			}
			peer.mu.Unlock()
			page, err := reads.List(t.Context(), generationAPIUser, "", 20)
			if tc.degrade {
				if err != nil || !reflect.DeepEqual(page.Messages[1].Answer.Source.ChangedSince, []string{"unverified"}) || page.Messages[1].Answer.Blocks[0].Text != "saved" {
					t.Fatalf("outage: %+v %v", page, err)
				}
				if got := revisionWatch(t, e, reads)[0].Answer.Source; !reflect.DeepEqual(got.ChangedSince, []string{"unverified"}) {
					t.Fatalf("outage snapshot: %+v", got)
				}
			} else if err == nil || strings.Contains(err.Error(), "PRIVATE") {
				t.Fatalf("refusal: %+v %v", page, err)
			}
			if !tc.degrade {
				sent := false
				err = reads.Watch(t.Context(), generationAPIUser, e.task.AnswerID, func(dialogue.Event) error { sent = true; return nil })
				if err == nil || sent {
					t.Fatal("refusal emitted a snapshot", err)
				}
			}
			var raw []byte
			if err := e.db.Primary().QueryRow(t.Context(), `SELECT data_revisions FROM answer WHERE id=$1`, e.task.AnswerID).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			decoded, err := datarevision.DecodeBaseline(raw)
			if err != nil || !reflect.DeepEqual(decoded, revisionBaseline(1)) {
				t.Fatal("outage overwrote baseline", decoded, err)
			}
		})
	}
	peer.mu.Lock()
	peer.read = nil
	peer.revisions = strictAPIWire(1, 1, 1)
	peer.mu.Unlock()
	if got := revisionHistory(t, reads).Source; len(got.ChangedSince) != 0 {
		t.Fatal("recovered peer did not recover proof", got)
	}
	peer.mu.Lock()
	peer.read = func(context.Context, *statementv1.GetDataVersionRequest) (*statementv1.GetDataVersionResponse, error) {
		return &statementv1.GetDataVersionResponse{Version: &statementv1.DataVersion{Revisions: strictAPIWire(1, 1, 1), CategoriesChangedAt: &timestamppb.Timestamp{Seconds: 1 << 62}, UploadsChangedAt: &timestamppb.Timestamp{Seconds: 1 << 62}}}, nil
	}
	peer.mu.Unlock()
	if got := revisionHistory(t, reads).Source; len(got.ChangedSince) != 0 {
		t.Fatal("obsolete timestamp vetoed revisions", got)
	}
	if got := revisionWatch(t, e, reads)[0].Answer.Source; len(got.ChangedSince) != 0 {
		t.Fatal("obsolete timestamp vetoed snapshot", got)
	}
	peer.mu.Lock()
	peer.read = nil
	peer.revisions = []*commonv1.DataRevision{{Code: "uploads", Value: -1}}
	peer.mu.Unlock()
	if _, err := reads.List(t.Context(), generationAPIUser, "", 20); err == nil {
		t.Fatal("malformed current vector certified history")
	}
	sent := false
	if err := reads.Watch(t.Context(), generationAPIUser, e.task.AnswerID, func(dialogue.Event) error { sent = true; return nil }); err == nil || sent {
		t.Fatal("malformed current vector emitted a snapshot", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := reads.List(ctx, generationAPIUser, "", 20); !errors.Is(err, context.Canceled) {
		t.Fatal("parent cancellation degraded", err)
	}
}

func TestIntegrationRevisionDialogueDelayedCommitAndLiveSource(t *testing.T) {
	for _, initial := range []bool{false, true} {
		t.Run(map[bool]string{false: "first-source", true: "same-source-refresh"}[initial], func(t *testing.T) {
			e := newRevisionComposition(t)
			e.exec(t, `CREATE TABLE revision_primary_fixture(uploads bigint NOT NULL,changed_at timestamptz NOT NULL)`)
			stamp := generationAPINow.Add(-time.Hour)
			e.exec(t, `INSERT INTO revision_primary_fixture VALUES(5,$1)`, stamp)
			writer, err := e.db.Primary().Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = writer.Rollback(context.Background()) }()
			if _, err = writer.Exec(t.Context(), `UPDATE revision_primary_fixture SET uploads=6`); err != nil {
				t.Fatal(err)
			}
			reader, err := e.db.Primary().BeginTx(t.Context(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = reader.Rollback(context.Background()) }()
			var old int64
			if err = reader.QueryRow(t.Context(), `SELECT uploads FROM revision_primary_fixture`).Scan(&old); err != nil || old != 5 {
				t.Fatal(old, err)
			}
			base, _ := json.Marshal(revisionBaseline(old))
			if initial {
				e.exec(t, `UPDATE answer SET calculated_at=$2,data_revisions=$3 WHERE id=$1`, e.task.AnswerID, generationAPINow, base)
			}
			peer, target := startDialogueVersionPeer(t)
			peer.read = func(ctx context.Context, _ *statementv1.GetDataVersionRequest) (*statementv1.GetDataVersionResponse, error) {
				var current int64
				var changed time.Time
				if err := e.db.Primary().QueryRow(ctx, `SELECT uploads,changed_at FROM revision_primary_fixture`).Scan(&current, &changed); err != nil {
					return nil, err
				}
				return &statementv1.GetDataVersionResponse{Version: &statementv1.DataVersion{Revisions: strictAPIWire(5, current, 5), UploadsChangedAt: timestamppb.New(changed)}}, nil
			}
			reads := e.reads(client.NewDataVersions(statementv1.NewAssistantToolsServiceClient(revisionNeighbor(t, target))))
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			events := make(chan dialogue.Event, 8)
			done := make(chan error, 1)
			go func() {
				done <- reads.Watch(ctx, generationAPIUser, e.task.AnswerID, func(event dialogue.Event) error { events <- event; return nil })
			}()
			select {
			case first := <-events:
				if first.Kind != "snapshot" || initial && len(first.Answer.Source.ChangedSince) != 0 {
					t.Fatal(first)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if err = writer.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
			var retained int64
			if err = reader.QueryRow(t.Context(), `SELECT uploads FROM revision_primary_fixture`).Scan(&retained); err != nil || retained != old {
				t.Fatal("RR did not retain old facts", retained, err)
			}
			if err = reader.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
			e.finish(t, revisionBaseline(retained))
			select {
			case source := <-events:
				if source.Kind != "chunk" {
					t.Fatal(source)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			select {
			case source := <-events:
				if source.Kind != "source" || !reflect.DeepEqual(source.Source.ChangedSince, []string{"uploads"}) {
					t.Fatal("live source omitted primary comparison", source)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			select {
			case terminal := <-events:
				if terminal.Kind != "done" {
					t.Fatal(terminal)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if err = <-done; err != nil {
				t.Fatal(err)
			}
			e.exec(t, `UPDATE revision_primary_fixture SET uploads=7,changed_at=$1`, stamp.Add(-24*time.Hour))
			if got := revisionHistory(t, reads).Source; !reflect.DeepEqual(got.ChangedSince, []string{"uploads"}) {
				t.Fatal("clock rollback hid changed facts", got)
			}
		})
	}
}
