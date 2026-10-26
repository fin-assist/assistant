package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	grpcclient "github.com/fin-assist/go-grpc/client"

	assistantv1 "github.com/fin-assist/contracts/gen/go/finassist/assistant/v1"
	statementv1 "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	"github.com/fin-assist/contracts/gen/go/finerr"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"

	"github.com/fin-assist/go-grpc/reqctx"

	"github.com/fin-assist/assistant/internal/account"
	"github.com/fin-assist/assistant/internal/adapter/client"
	accountgrpc "github.com/fin-assist/assistant/internal/adapter/grpc/account"
	dialoguegrpc "github.com/fin-assist/assistant/internal/adapter/grpc/dialogue"
	"github.com/fin-assist/assistant/internal/adapter/postgres"
	"github.com/fin-assist/assistant/internal/dialogue"
	"github.com/fin-assist/assistant/internal/domain"
	"github.com/fin-assist/assistant/internal/generation"
)

type storedDialogueStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s storedDialogueStream) Context() context.Context { return s.ctx }

type storedDialogueAllowance struct{ service *account.Service }

func (a storedDialogueAllowance) Remaining(ctx context.Context, user string) (int, error) {
	limit, err := a.service.GetLimit(ctx, user)
	return limit.Remaining, err
}

// Assemble just dialogue transport/usecase/storage on a separate server. The fixed
// trusted subject is a fixture boundary, not a production authentication check.
// Production's full identity/access/asking assembly is exercised by the API suite.
func storedDialogueClient(t *testing.T, e *acceptanceIntegration) assistantv1.AssistantServiceClient {
	t.Helper()
	notifications, err := postgres.NewAnswerNotifications(t.Context(), e.env.App.DB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(notifications.Close)
	peer, err := grpcclient.Dial("passthrough:///statements", grpcclient.Options{Peer: "bank-statement-data", Tokens: grpcclient.DevToken("assistant"), DialOptions: generationAPIPeerOptions(t, e.peers)["bank-statement-data"]})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = peer.Close() })
	accounts := account.New(e.env.App.DB, postgres.NewAccounts(e.env.App.DB), e.env.App.Tombstones, func() time.Time { return *e.now }, 5)
	reads := dialogue.New(postgres.NewDialogue(e.env.App.DB), client.NewDataVersions(statementv1.NewAssistantToolsServiceClient(peer)), notifications, storedDialogueAllowance{accounts})
	server := grpc.NewServer(grpc.UnaryInterceptor(func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
		return next(reqctx.WithUserID(ctx, generationAPIUser), req)
	}), grpc.StreamInterceptor(func(srv any, stream grpc.ServerStream, _ *grpc.StreamServerInfo, next grpc.StreamHandler) error {
		return next(srv, storedDialogueStream{stream, reqctx.WithUserID(stream.Context(), generationAPIUser)})
	}))
	accountgrpc.Register(server, accounts, dialoguegrpc.New(reads), accountgrpc.AskingOptions{UseCases: e.service, Metadata: e.keys})
	listener := bufconn.Listen(1 << 20)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient("passthrough:///dialogue-storage", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return assistantv1.NewAssistantServiceClient(conn)
}
func storedDialogueTask(t *testing.T, e *acceptanceIntegration) domain.GenerationTask {
	t.Helper()
	reply, err := e.service.Ask(t.Context(), generationAPIUser, acceptanceKey("ask", "transport storage"), "transport storage", "with")
	if err != nil {
		t.Fatal(err)
	}
	return reply.Task
}
func storedDialogueFinish(t *testing.T, e *acceptanceIntegration, task domain.GenerationTask, out generation.Outcome) {
	t.Helper()
	err := e.env.App.DB.Run(t.Context(), func(ctx context.Context) error {
		result, err := postgres.NewGenerations(e.env.App.DB).Finish(ctx, task, generation.PrepareFinish(out, *e.now))
		if err == nil && !result.Applied {
			t.Error("terminal write was not applied")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}
func TestIntegrationStoredDialogueCatalogPrecisionAndNavigation(t *testing.T) {
	e := newAcceptanceIntegration(t, 5)
	task := storedDialogueTask(t, e)
	c := storedDialogueClient(t, e)
	from, to := generationAPINow.AddDate(0, -1, 0), generationAPINow
	rows := `{"alt_text":"precise figures","data":{"rows":[{"label":"amount","value_kind":"amount","value":9223372036854775807},{"label":"count","sublabel":"items","value_kind":"count","value":7},{"label":"percent","value_kind":"percent","value":0.25},{"label":"text","value_kind":"text","value":"nothing"},{"label":"future","value_kind":"future","text":"fallback"}]}}`
	chart := `{"title":"trend","alt_text":"partial trend","data":{"kind":"bar","value_kind":"amount","highlight_index":0,"points":[{"range":{"from":"2026-09-01T00:00:00Z","to":"2026-10-01T00:00:00Z"},"value":12345,"coverage":"partial","data_from":"2026-09-01T00:00:00Z","data_to":"2026-09-20T00:00:00Z"},{"range":{"from":"2026-10-01T00:00:00Z","to":"2026-11-01T00:00:00Z"},"value":null,"coverage":"no_data"}]}}`
	chips := []json.RawMessage{json.RawMessage(`{"label":"operations","screen":"operations","filters":{"from":"2026-09-01T00:00:00Z","to":"2026-10-01T00:00:00Z","q":"saved search","category_id":"category","kind":"expense","amount_from":1,"amount_to":9223372036854775807,"transfer_mode":"with","selection":"selected","selection_name":"selection"}}`), json.RawMessage(`{"label":"quarter","screen":"analytics","params":{"period":"quarter","date":"2026-Q3","transfer_mode":"without"}}`)}
	storedDialogueFinish(t, e, task, generation.Outcome{Charged: true, Blocks: []dialogue.Block{{Type: "text", Version: 1, Text: "saved"}, {Type: "rows", Version: 1, Body: json.RawMessage(rows)}, {Type: "chart", Version: 1, Body: json.RawMessage(chart)}, {Type: "future", Version: 9, Body: json.RawMessage(`{"alt_text":"future equivalent","data":{"secret":"hidden"}}`)}}, Chips: chips, Source: &dialogue.Source{CalculatedAt: generationAPINow, From: &from, To: &to, TransferMode: "with"}})
	page, err := c.ListMessages(t.Context(), &assistantv1.ListMessagesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 2 {
		t.Fatalf("history=%v", page)
	}
	answer := page.Messages[1]
	if len(answer.Blocks) != 4 || answer.Blocks[1].GetRows().Rows[0].GetAmountMinor() != 9223372036854775807 || answer.Blocks[1].GetRows().Rows[1].GetCount() != 7 || answer.Blocks[1].GetRows().Rows[2].GetPercent() != 0.25 || answer.Blocks[1].GetRows().Rows[3].GetText() != "nothing" || answer.Blocks[3].GetText().GetText() != "future equivalent" {
		t.Fatalf("stored catalog lost precision/fallback: %v", answer)
	}
	if answer.Blocks[2].GetChart() == nil || len(answer.Blocks[2].GetChart().Points) != 2 || answer.Blocks[2].GetChart().Points[0].GetValue() != 12345 || answer.Blocks[2].GetChart().Points[1].Value != nil || answer.GetSource() == nil || len(answer.Chips) != 2 || answer.Chips[0].Filters.GetAmountToMinor() != 9223372036854775807 || answer.Chips[1].Params.PeriodKey != "2026-Q3" {
		t.Fatalf("stored chart/navigation=%v", answer)
	}
	stream, err := c.WatchAnswer(t.Context(), &assistantv1.WatchAnswerRequest{AnswerId: task.AnswerID})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := stream.Recv()
	if err != nil || snapshot.GetSnapshot() == nil || len(snapshot.GetSnapshot().Blocks) != 4 {
		t.Fatalf("snapshot=%v %v", snapshot, err)
	}
	done, err := stream.Recv()
	if err != nil || done.GetDone() == nil || done.GetDone().GetRemainingLimit() != 4 {
		t.Fatalf("done=%v %v", done, err)
	}
	if _, err = stream.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("terminal stream=%v", err)
	}
	if e.peers.count("version") != 2 {
		t.Fatalf("version reads=%d", e.peers.count("version"))
	}
}
func TestIntegrationStoredDialogueStreamsOnlyCommittedExtensions(t *testing.T) {
	e := newAcceptanceIntegration(t, 5)
	task := storedDialogueTask(t, e)
	c := storedDialogueClient(t, e)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	stream, err := c.WatchAnswer(ctx, &assistantv1.WatchAnswerRequest{AnswerId: task.AnswerID})
	if err != nil {
		t.Fatal(err)
	}
	first, err := stream.Recv()
	if err != nil || first.GetSnapshot() == nil {
		t.Fatalf("first=%v %v", first, err)
	}
	repo := postgres.NewGenerations(e.env.App.DB)
	appendText := func(position int, text string) {
		t.Helper()
		if err := e.env.App.DB.Run(t.Context(), func(ctx context.Context) error { return repo.Append(ctx, task, position, text) }); err != nil {
			t.Fatal(err)
		}
	}
	appendText(0, "first")
	chunk, err := stream.Recv()
	if err != nil || chunk.GetChunk().GetText() != "first" || chunk.GetChunk().GetIndex() != 0 {
		t.Fatalf("first chunk=%v %v", chunk, err)
	}
	appendText(0, " extension")
	chunk, err = stream.Recv()
	if err != nil || chunk.GetChunk().GetText() != " extension" {
		t.Fatalf("extension=%v %v", chunk, err)
	}
	block := dialogue.Block{Type: "rows", Version: 1, Body: json.RawMessage(`{"alt_text":"rows","data":{"rows":[{"label":"count","value_kind":"count","value":2}]}}`)}
	if err := e.env.App.DB.Run(t.Context(), func(ctx context.Context) error { return repo.AppendBlock(ctx, task, 1, block) }); err != nil {
		t.Fatal(err)
	}
	event, err := stream.Recv()
	if err != nil || event.GetBlock() == nil || event.GetBlock().Index != 1 || event.GetBlock().Block.GetRows().Rows[0].GetCount() != 2 {
		t.Fatalf("block=%v %v", event, err)
	}
	from, to := e.now.AddDate(0, -1, 0), *e.now
	storedDialogueFinish(t, e, task, generation.Outcome{Charged: true, Blocks: []dialogue.Block{{Type: "text", Version: 1, Text: "first extension"}, block}, Source: &dialogue.Source{CalculatedAt: *e.now, From: &from, To: &to, TransferMode: "with"}, Chips: []json.RawMessage{json.RawMessage(`{"label":"year","screen":"analytics","params":{"period":"year","date":"2026","transfer_mode":"with"}}`)}})
	source, err := stream.Recv()
	if err != nil || source.GetSource() == nil {
		t.Fatalf("source=%v %v", source, err)
	}
	chip, err := stream.Recv()
	if err != nil || chip.GetChip() == nil || chip.GetChip().GetParams().GetPeriodKey() != "2026" {
		t.Fatalf("chip=%v %v", chip, err)
	}
	done, err := stream.Recv()
	if err != nil || done.GetDone() == nil || done.GetDone().GetRemainingLimit() != 4 {
		t.Fatalf("done=%v %v", done, err)
	}
	if _, err = stream.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("after done=%v", err)
	}
}

func TestIntegrationStoredDialogueFailureDiscardsPublishedPartial(t *testing.T) {
	for _, code := range []string{"MODEL_ERROR", "CANNOT_ANSWER", "OFF_TOPIC"} {
		t.Run(code, func(t *testing.T) {
			e := newAcceptanceIntegration(t, 5)
			task := storedDialogueTask(t, e)
			c := storedDialogueClient(t, e)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			stream, err := c.WatchAnswer(ctx, &assistantv1.WatchAnswerRequest{AnswerId: task.AnswerID})
			if err != nil {
				t.Fatal(err)
			}
			if snapshot, err := stream.Recv(); err != nil || snapshot.GetSnapshot() == nil {
				t.Fatalf("snapshot=%v %v", snapshot, err)
			}
			if err := e.env.App.DB.Run(t.Context(), func(ctx context.Context) error {
				return postgres.NewGenerations(e.env.App.DB).Append(ctx, task, 0, "partial must disappear")
			}); err != nil {
				t.Fatal(err)
			}
			if chunk, err := stream.Recv(); err != nil || chunk.GetChunk().GetText() != "partial must disappear" {
				t.Fatalf("chunk=%v %v", chunk, err)
			}
			storedDialogueFinish(t, e, task, generation.Outcome{ErrorCode: code})
			failure, err := stream.Recv()
			if err != nil || failure.GetError() == nil || failure.GetError().GetCode().String() != "ANSWER_ERROR_CODE_"+code || failure.GetError().GetRemainingLimit() != 5 || failure.GetError().GetRetryable() != (code == "MODEL_ERROR") {
				t.Fatalf("failure=%v %v", failure, err)
			}
			canonical, valid := finerr.DecodeResultStatus(failure.GetError().GetStatus())
			classification := codes.FailedPrecondition
			if code == "MODEL_ERROR" {
				classification = codes.Internal
			}
			if !valid || canonical.Reason != code || canonical.Code != classification || len(failure.GetError().GetStatus().GetDetails()) != 1 {
				t.Fatalf("invalid canonical failure: %v", failure)
			}
			if _, err = stream.Recv(); !errors.Is(err, io.EOF) {
				t.Fatalf("after failure=%v", err)
			}
			page, err := c.ListMessages(t.Context(), &assistantv1.ListMessagesRequest{})
			if err != nil || len(page.GetMessages()) != 2 || len(page.Messages[1].GetBlocks()) != 0 || page.Messages[1].GetErrorCode().String() != "ANSWER_ERROR_CODE_"+code {
				t.Fatalf("failed history exposed partial=%v %v", page, err)
			}
			if !proto.Equal(page.Messages[1].GetError(), failure.GetError().GetStatus()) || page.Messages[1].Retryable == nil || page.Messages[1].GetRetryable() != failure.GetError().GetRetryable() {
				t.Fatalf("saved and streamed failure disagree: %v / %v", page, failure)
			}
			e.assertCounts(t, 0, 1, 1)
		})
	}
}

func TestIntegrationStoredCatalogRejectsValuesUnrepresentableOnWire(t *testing.T) {
	e := newAcceptanceIntegration(t, 5)
	task := storedDialogueTask(t, e)
	c := storedDialogueClient(t, e)
	storedDialogueFinish(t, e, task, generation.Outcome{Charged: true, Blocks: []dialogue.Block{{Type: "rows", Version: 1, Body: json.RawMessage(`{"alt_text":"figures","data":{"rows":[]}}`)}}})
	for _, tc := range []struct{ name, kind, body string }{
		{"fractional-minor-unit", "rows", `{"rows":[{"label":"private","value_kind":"amount","value":1.5}]}`},
		{"integer-overflow", "rows", `{"rows":[{"label":"private","value_kind":"amount","value":9223372036854775808}]}`},
		{"negative-count", "rows", `{"rows":[{"label":"private","value_kind":"count","value":-1}]}`},
		{"percent-outside-range", "rows", `{"rows":[{"label":"private","value_kind":"percent","value":1.1}]}`},
		{"missing-value", "rows", `{"rows":[{"label":"private","value_kind":"count"}]}`},
		{"missing-label", "rows", `{"rows":[{"value_kind":"count","value":1}]}`},
		{"invalid-text", "rows", `{"rows":[{"label":"private","value_kind":"text","value":17}]}`},
		{"unsafe-double", "chart", `{"kind":"bar","value_kind":"amount","points":[{"range":{"from":"2026-09-01T00:00:00Z","to":"2026-10-01T00:00:00Z"},"coverage":"complete","value":9007199254740992}]}`},
		{"reversed-range", "chart", `{"kind":"bar","value_kind":"percent","points":[{"range":{"from":"2026-10-01T00:00:00Z","to":"2026-09-01T00:00:00Z"},"coverage":"complete","value":0.1}]}`},
		{"bad-highlight", "chart", `{"kind":"bar","value_kind":"amount","highlight_index":1,"points":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"alt_text":"PRIVATE financial details","data":` + tc.body + `}`
			e.exec(t, `UPDATE answer_block SET type=$3,body=$4 WHERE user_id=$1 AND answer_id=$2`, generationAPIUser, task.AnswerID, tc.kind, []byte(body))
			page, err := c.ListMessages(t.Context(), &assistantv1.ListMessagesRequest{})
			if err == nil || page != nil || strings.Contains(err.Error(), "PRIVATE") {
				t.Fatalf("invalid saved catalog exposed data: page=%v err=%v", page, err)
			}
		})
	}
}

func TestIntegrationQuestionTransportCommitsAndReplaysSavedAcceptance(t *testing.T) {
	e := newAcceptanceIntegration(t, 5)
	c := storedDialogueClient(t, e)
	ctx := metadata.AppendToOutgoingContext(t.Context(), "idempotency-key", uuid.NewString())
	req := &assistantv1.AskQuestionRequest{Text: "stored transport question"}
	first, err := c.AskQuestion(ctx, req)
	if err != nil || first.GetAnswerId() == "" || first.GetQuestion().GetText() != req.Text || first.GetRemainingLimit() != 4 {
		t.Fatalf("first=%v %v", first, err)
	}
	replay, err := c.AskQuestion(ctx, req)
	if err != nil || replay.GetAnswerId() != first.AnswerId || replay.GetQuestion().GetId() != first.Question.Id || e.starter.count() != 1 {
		t.Fatalf("replay=%v %v", replay, err)
	}
	retryCtx := metadata.AppendToOutgoingContext(t.Context(), "idempotency-key", uuid.NewString())
	retryReq := &assistantv1.RetryAnswerRequest{AnswerId: first.AnswerId}
	if _, err := c.RetryAnswer(retryCtx, retryReq); err == nil {
		t.Fatal("generating retry accepted")
	}
	e.assertCounts(t, 1, 1, 1)
	e.exec(t, `UPDATE answer SET status='failed',charged=false,error_code='MODEL_ERROR',retryable=true WHERE user_id=$1 AND id=$2`, generationAPIUser, first.AnswerId)
	e.exec(t, `UPDATE user_state SET limit_used=0 WHERE user_id=$1`, generationAPIUser)
	retry, err := c.RetryAnswer(retryCtx, retryReq)
	if err != nil || retry.GetAnswerId() == first.AnswerId || retry.GetAnswerId() == "" || retry.GetRemainingLimit() != 4 {
		t.Fatalf("retry=%v %v", retry, err)
	}
	replayRetry, err := c.RetryAnswer(retryCtx, retryReq)
	if err != nil || replayRetry.GetAnswerId() != retry.AnswerId || e.starter.count() != 2 {
		t.Fatalf("retryreplay=%v %v", replayRetry, err)
	}
	e.assertCounts(t, 1, 1, 2)
}
