package app_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/timestamppb"

	assistantv1 "github.com/fin-assist/contracts/gen/go/finassist/assistant/v1"
	statementv1 "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	deletionv1 "github.com/fin-assist/contracts/gen/go/finassist/userdeletion/v1"

	"github.com/fin-assist/assistant/internal/app"
	"github.com/fin-assist/assistant/internal/app/apitest"
	"github.com/fin-assist/assistant/internal/config"
)

const apiDialogueUser = "10000000-0000-0000-0000-000000000001"
const apiDialogueOther = "10000000-0000-0000-0000-000000000002"
const apiDialogueQuestion = "20000000-0000-0000-0000-000000000001"
const apiDialogueAnswer = "30000000-0000-0000-0000-000000000001"

func dialogueAPIExec(t *testing.T, env *apitest.Env, query string, args ...any) {
	t.Helper()
	if _, err := env.App.DB.Writer(t.Context()).Exec(t.Context(), query, args...); err != nil {
		t.Fatal(err)
	}
}
func seedAPIDialogue(t *testing.T, env *apitest.Env, state string) {
	t.Helper()
	day := time.Now().In(time.FixedZone("Moscow", 3*60*60)).Format("2006-01-02")
	dialogueAPIExec(t, env, `INSERT INTO user_state(user_id,time_zone,limit_day,limit_used) VALUES ($1,'Asia/Tokyo',$2,1)`, apiDialogueUser, day)
	dialogueAPIExec(t, env, `INSERT INTO question(user_id,id,text,transfer_mode,created_at) VALUES ($1,$2,'question','without','2026-10-23T12:00:00Z')`, apiDialogueUser, apiDialogueQuestion)
	dialogueAPIExec(t, env, `INSERT INTO answer(user_id,id,question_id,charge_day,status,created_at) VALUES ($1,$2,$3,$4,$5,'2026-10-23T12:00:00Z')`, apiDialogueUser, apiDialogueAnswer, apiDialogueQuestion, day, state)
	dialogueAPIExec(t, env, `INSERT INTO answer_block(user_id,answer_id,position,type,schema_version,markdown) VALUES ($1,$2,0,'text',1,'saved')`, apiDialogueUser, apiDialogueAnswer)
}
func apiDialogueContext(t *testing.T, user string) (context.Context, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	return apitest.User(ctx, user), cancel
}

func TestDialogueAPIHistoryPaginationAndOwnership(t *testing.T) {
	env := apitest.Start(t)
	seedAPIDialogue(t, env, "complete")
	c := assistantv1.NewAssistantServiceClient(env.Conn)
	ctx, cancel := apiDialogueContext(t, apiDialogueUser)
	defer cancel()
	p, err := c.ListMessages(ctx, &assistantv1.ListMessagesRequest{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Messages) != 1 || !p.HasOlder || p.TimeZone != "Asia/Tokyo" || p.Messages[0].Id != apiDialogueAnswer || p.Messages[0].GetRole() != assistantv1.Role_ROLE_ASSISTANT || p.Messages[0].GetBlocks()[0].GetText().GetText() != "saved" || !p.Messages[0].GetCharged() {
		t.Fatalf("latest: %v", p)
	}
	before := apiDialogueAnswer
	p, err = c.ListMessages(ctx, &assistantv1.ListMessagesRequest{BeforeId: &before, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Messages) != 1 || p.HasOlder || p.Messages[0].Id != apiDialogueQuestion || p.Messages[0].GetText() != "question" {
		t.Fatalf("older: %v", p)
	}
	p, err = c.ListMessages(ctx, &assistantv1.ListMessagesRequest{})
	if err != nil || len(p.GetMessages()) != 2 || p.Messages[0].Id != apiDialogueQuestion {
		t.Fatalf("default page: %v %v", p, err)
	}
	other, cancelOther := apiDialogueContext(t, apiDialogueOther)
	defer cancelOther()
	p, err = c.ListMessages(other, &assistantv1.ListMessagesRequest{})
	if err != nil || len(p.GetMessages()) != 0 || p.GetTimeZone() != "Europe/Moscow" {
		t.Fatalf("foreign page: %v %v", p, err)
	}
	dialogueAPIExec(t, env, `UPDATE answer SET replaced_at=now() WHERE user_id=$1 AND id=$2`, apiDialogueUser, apiDialogueAnswer)
	p, err = c.ListMessages(ctx, &assistantv1.ListMessagesRequest{})
	if err != nil || len(p.GetMessages()) != 1 || p.Messages[0].Id != apiDialogueQuestion {
		t.Fatalf("replaced page: %v %v", p, err)
	}
	_, err = deletionv1.NewUserDeletionServiceClient(env.Conn).PurgeUserData(apitest.Caller(t.Context(), "user"), &deletionv1.PurgeUserDataRequest{UserId: apiDialogueUser})
	if err != nil {
		t.Fatal(err)
	}
	p, err = c.ListMessages(ctx, &assistantv1.ListMessagesRequest{})
	if status.Code(err) != codes.Unauthenticated || apitest.Reason(err) != "TOKEN_REVOKED" {
		t.Fatalf("deleted user admission: %v %v", p, err)
	}
}

func TestDialogueAPIValidationAndAccess(t *testing.T) {
	env := apitest.Start(t)
	c := assistantv1.NewAssistantServiceClient(env.Conn)
	ctx, cancel := apiDialogueContext(t, apiDialogueUser)
	defer cancel()
	bad := "not-a-uuid"
	empty := ""
	for _, req := range []*assistantv1.ListMessagesRequest{{Limit: -1}, {Limit: 51}, {BeforeId: &bad}, {BeforeId: &empty}} {
		_, err := c.ListMessages(ctx, req)
		if status.Code(err) != codes.InvalidArgument || apitest.Reason(err) != "VALIDATION_ERROR" {
			t.Fatalf("request %v: %v", req, err)
		}
	}
	for _, id := range []string{"", "invalid"} {
		s, err := c.WatchAnswer(ctx, &assistantv1.WatchAnswerRequest{AnswerId: id})
		if err == nil {
			_, err = s.Recv()
		}
		if status.Code(err) != codes.InvalidArgument || apitest.Reason(err) != "VALIDATION_ERROR" {
			t.Fatalf("invalid watch: %v", err)
		}
	}
	_, err := c.ListMessages(t.Context(), &assistantv1.ListMessagesRequest{})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("missing token: %v", err)
	}
	wrong := metadata.NewOutgoingContext(t.Context(), metadata.Pairs("x-service-authorization", "Bearer dev:user", "authorization", "Bearer dev:"+apiDialogueUser))
	_, err = c.ListMessages(wrong, &assistantv1.ListMessagesRequest{})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("foreign caller: %v", err)
	}
	ctxCancelled, cancelNow := context.WithCancel(ctx)
	cancelNow()
	_, err = c.ListMessages(ctxCancelled, &assistantv1.ListMessagesRequest{})
	if status.Code(err) != codes.Canceled {
		t.Fatalf("cancelled list: %v", err)
	}
}

func TestDialogueAPIWatchTerminalAndNotFound(t *testing.T) {
	for _, state := range []string{"complete", "failed"} {
		t.Run(state, func(t *testing.T) {
			env := apitest.Start(t)
			seedAPIDialogue(t, env, state)
			c := assistantv1.NewAssistantServiceClient(env.Conn)
			if state == "failed" {
				dialogueAPIExec(t, env, `UPDATE answer SET error_code='MODEL_ERROR',retryable=true,charged=false WHERE user_id=$1 AND id=$2`, apiDialogueUser, apiDialogueAnswer)
			}
			ctx, cancel := apiDialogueContext(t, apiDialogueUser)
			defer cancel()
			s, err := c.WatchAnswer(ctx, &assistantv1.WatchAnswerRequest{AnswerId: apiDialogueAnswer})
			if err != nil {
				t.Fatal(err)
			}
			first, err := s.Recv()
			if err != nil || first.GetSnapshot() == nil || first.GetTimeZone() != "Asia/Tokyo" {
				t.Fatalf("snapshot: %v %v", first, err)
			}
			terminal, err := s.Recv()
			if err != nil {
				t.Fatal(err)
			}
			if state == "complete" {
				if first.GetSnapshot().GetStatus() != assistantv1.AnswerStatus_ANSWER_STATUS_COMPLETE || terminal.GetDone() == nil || !terminal.GetDone().GetCharged() || terminal.GetDone().GetRemainingLimit() != 4 {
					t.Fatalf("complete: %v %v", first, terminal)
				}
			} else {
				if len(first.GetSnapshot().GetBlocks()) != 0 || first.GetSnapshot().GetStatus() != assistantv1.AnswerStatus_ANSWER_STATUS_FAILED || terminal.GetError().GetCode() != assistantv1.AnswerErrorCode_ANSWER_ERROR_CODE_MODEL_ERROR || !terminal.GetError().GetRetryable() || terminal.GetError().GetText() == "" {
					t.Fatalf("failed: %v %v", first, terminal)
				}
			}
			if _, err := s.Recv(); !errors.Is(err, io.EOF) {
				t.Fatalf("terminal stream did not close: %v", err)
			}
			other, cancelOther := apiDialogueContext(t, apiDialogueOther)
			defer cancelOther()
			for _, id := range []string{apiDialogueAnswer, "30000000-0000-0000-0000-000000000099"} {
				s, err := c.WatchAnswer(other, &assistantv1.WatchAnswerRequest{AnswerId: id})
				if err == nil {
					_, err = s.Recv()
				}
				if status.Code(err) != codes.NotFound || apitest.Reason(err) != "NOT_FOUND" {
					t.Fatalf("foreign/missing: %v", err)
				}
			}
			dialogueAPIExec(t, env, `UPDATE answer SET replaced_at=now() WHERE user_id=$1 AND id=$2`, apiDialogueUser, apiDialogueAnswer)
			s, err = c.WatchAnswer(ctx, &assistantv1.WatchAnswerRequest{AnswerId: apiDialogueAnswer})
			if err == nil {
				_, err = s.Recv()
			}
			if status.Code(err) != codes.NotFound {
				t.Fatalf("replaced: %v", err)
			}
		})
	}
}

// A second real assembly shares only PostgreSQL; its LISTEN connection is independent.
func dialogueReplica(t *testing.T, env *apitest.Env) *grpc.ClientConn {
	t.Helper()
	a, err := app.New(t.Context(), env.App.Cfg, slog.New(slog.DiscardHandler), prometheus.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	lis := bufconn.Listen(1 << 20)
	go func() { _ = a.GRPC.Serve(lis) }()
	conn, err := grpc.NewClient("passthrough:///dialogue-replica", grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		a.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a.GRPC.Shutdown(ctx, 0)
		a.Close()
	})
	return conn
}

func TestDialogueAPIWatchCommittedUpdateAcrossInstances(t *testing.T) {
	env := apitest.Start(t)
	seedAPIDialogue(t, env, "generating")
	conn := dialogueReplica(t, env)
	ctx, cancel := apiDialogueContext(t, apiDialogueUser)
	defer cancel()
	streams := make([]assistantv1.AssistantService_WatchAnswerClient, 0, 2)
	for _, conn := range []*grpc.ClientConn{env.Conn, conn} {
		s, err := assistantv1.NewAssistantServiceClient(conn).WatchAnswer(ctx, &assistantv1.WatchAnswerRequest{AnswerId: apiDialogueAnswer})
		if err != nil {
			t.Fatal(err)
		}
		snapshot, err := s.Recv()
		if err != nil || snapshot.GetSnapshot().GetBlocks()[0].GetText().GetText() != "saved" {
			t.Fatalf("initial: %v %v", snapshot, err)
		}
		streams = append(streams, s)
	}
	// The transaction is independent of either observing process.
	err := env.App.DB.Run(ctx, func(ctx context.Context) error {
		if _, err := env.App.DB.Writer(ctx).Exec(ctx, `UPDATE answer_block SET markdown='saved and committed' WHERE user_id=$1 AND answer_id=$2 AND position=0`, apiDialogueUser, apiDialogueAnswer); err != nil {
			return err
		}
		if _, err := env.App.DB.Writer(ctx).Exec(ctx, `UPDATE answer SET status='complete' WHERE user_id=$1 AND id=$2`, apiDialogueUser, apiDialogueAnswer); err != nil {
			return err
		}
		_, err := env.App.DB.Writer(ctx).Exec(ctx, `SELECT pg_notify('assistant_answers',$1)`, apiDialogueAnswer)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range streams {
		chunk, err := s.Recv()
		if err != nil || chunk.GetChunk().GetText() != " and committed" || chunk.GetChunk().GetIndex() != 0 {
			t.Fatalf("committed chunk: %v %v", chunk, err)
		}
		done, err := s.Recv()
		if err != nil || done.GetDone() == nil {
			t.Fatalf("done: %v %v", done, err)
		}
		if _, err := s.Recv(); !errors.Is(err, io.EOF) {
			t.Fatalf("EOF: %v", err)
		}
	}
}

func TestDialogueAPIWatchDeletionWakeAndCancellation(t *testing.T) {
	env := apitest.Start(t)
	seedAPIDialogue(t, env, "generating")
	c := assistantv1.NewAssistantServiceClient(env.Conn)
	ctx, cancel := apiDialogueContext(t, apiDialogueUser)
	defer cancel()
	s, err := c.WatchAnswer(ctx, &assistantv1.WatchAnswerRequest{AnswerId: apiDialogueAnswer})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Recv(); err != nil {
		t.Fatal(err)
	}
	cancelCtx, cancelStream := context.WithCancel(ctx)
	cancelled, err := c.WatchAnswer(cancelCtx, &assistantv1.WatchAnswerRequest{AnswerId: apiDialogueAnswer})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cancelled.Recv(); err != nil {
		t.Fatal(err)
	}
	cancelStream()
	if _, err := cancelled.Recv(); status.Code(err) != codes.Canceled {
		t.Fatalf("cancelled stream: %v", err)
	}
	_, err = deletionv1.NewUserDeletionServiceClient(env.Conn).PurgeUserData(apitest.Caller(t.Context(), "user"), &deletionv1.PurgeUserDataRequest{UserId: apiDialogueUser})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Recv(); status.Code(err) != codes.NotFound || apitest.Reason(err) != "NOT_FOUND" {
		t.Fatalf("deletion must wake stream: %v", err)
	}
}

type dialogueVersionPeer struct {
	statementv1.UnimplementedAssistantToolsServiceServer
	mu       sync.Mutex
	calls    int
	users    []string
	metadata []metadata.MD
	fail     bool
}

func (p *dialogueVersionPeer) GetDataVersion(ctx context.Context, req *statementv1.GetDataVersionRequest) (*statementv1.GetDataVersionResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.users = append(p.users, req.GetUserId())
	md, _ := metadata.FromIncomingContext(ctx)
	p.metadata = append(p.metadata, md.Copy())
	if p.fail {
		return nil, errors.New("private peer secret with financial data")
	}
	calculated := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	return &statementv1.GetDataVersionResponse{Version: &statementv1.DataVersion{CategoriesChangedAt: timestamppb.New(calculated), UploadsChangedAt: timestamppb.New(calculated.Add(time.Second))}}, nil
}
func startDialogueVersionPeer(t *testing.T) (*dialogueVersionPeer, string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &dialogueVersionPeer{}
	server := grpc.NewServer()
	statementv1.RegisterAssistantToolsServiceServer(server, p)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	return p, listener.Addr().String()
}
func setAPIDialogueSource(t *testing.T, env *apitest.Env) {
	t.Helper()
	dialogueAPIExec(t, env, `UPDATE answer SET calculated_at='2026-10-23T12:00:00Z',data_from='2026-10-01T00:00:00Z',data_to='2026-11-01T00:00:00Z' WHERE user_id=$1`, apiDialogueUser)
}

func TestDialogueAPISourceHistoryAndWatchPeer(t *testing.T) {
	peer, target := startDialogueVersionPeer(t)
	env := apitest.Start(t, func(c *config.Config) { c.Peers["bank-statement-data"] = target })
	seedAPIDialogue(t, env, "complete")
	setAPIDialogueSource(t, env)
	dialogueAPIExec(t, env, `INSERT INTO question(user_id,id,text,transfer_mode) VALUES ($1,'20000000-0000-0000-0000-000000000002','second','without')`, apiDialogueUser)
	dialogueAPIExec(t, env, `INSERT INTO answer(user_id,id,question_id,charge_day,status,calculated_at,data_from,data_to) SELECT user_id,'30000000-0000-0000-0000-000000000002','20000000-0000-0000-0000-000000000002',charge_day,'complete',calculated_at,data_from,data_to FROM answer WHERE user_id=$1 AND id=$2`, apiDialogueUser, apiDialogueAnswer)
	c := assistantv1.NewAssistantServiceClient(env.Conn)
	ctx, cancel := apiDialogueContext(t, apiDialogueUser)
	defer cancel()
	page, err := c.ListMessages(ctx, &assistantv1.ListMessagesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 4 {
		t.Fatalf("source page: %v", page)
	}
	for _, m := range page.Messages {
		if m.GetRole() != assistantv1.Role_ROLE_ASSISTANT {
			continue
		}
		source := m.GetSource()
		if source == nil || len(source.GetChangedSince()) != 1 || source.GetChangedSince()[0] != assistantv1.ChangedSince_CHANGED_SINCE_UPLOADS {
			t.Fatalf("strict-after source: %v", source)
		}
	}
	peer.mu.Lock()
	calls := peer.calls
	peer.mu.Unlock()
	if calls != 1 {
		t.Fatalf("history peer calls=%d, want one for whole page", calls)
	}
	stream, err := c.WatchAnswer(ctx, &assistantv1.WatchAnswerRequest{AnswerId: apiDialogueAnswer})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := stream.Recv()
	if err != nil || len(snapshot.GetSnapshot().GetSource().GetChangedSince()) != 1 || snapshot.GetSnapshot().GetSource().GetChangedSince()[0] != assistantv1.ChangedSince_CHANGED_SINCE_UPLOADS {
		t.Fatalf("stream source: %v %v", snapshot, err)
	}
	if terminal, err := stream.Recv(); err != nil || terminal.GetDone() == nil {
		t.Fatalf("terminal: %v %v", terminal, err)
	}
	peer.mu.Lock()
	defer peer.mu.Unlock()
	if peer.calls != 2 {
		t.Fatalf("source watch called peer %d times total, want2", peer.calls)
	}
	for i, user := range peer.users {
		if user != apiDialogueUser || len(peer.metadata[i].Get("authorization")) != 0 || len(peer.metadata[i].Get("x-service-authorization")) != 1 || peer.metadata[i].Get("x-service-authorization")[0] != "Bearer dev:apitest" {
			t.Fatalf("peer identity metadata call%d: user=%q metadata=%v", i, user, peer.metadata[i])
		}
	}
}

func TestDialogueAPISourcePeerFailuresSanitized(t *testing.T) {
	for _, configured := range []bool{false, true} {
		t.Run(map[bool]string{false: "unconfigured", true: "failed"}[configured], func(t *testing.T) {
			mutate := func(*config.Config) {}
			if configured {
				peer, target := startDialogueVersionPeer(t)
				peer.fail = true
				mutate = func(c *config.Config) { c.Peers["bank-statement-data"] = target }
			}
			env := apitest.Start(t, mutate)
			seedAPIDialogue(t, env, "complete")
			c := assistantv1.NewAssistantServiceClient(env.Conn)
			ctx, cancel := apiDialogueContext(t, apiDialogueUser)
			defer cancel()
			// A missing peer must not break dialogue that does not contain a source.
			if _, err := c.ListMessages(ctx, &assistantv1.ListMessagesRequest{}); err != nil {
				t.Fatal(err)
			}
			setAPIDialogueSource(t, env)
			_, err := c.ListMessages(ctx, &assistantv1.ListMessagesRequest{})
			if status.Code(err) != codes.Internal || strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "configured") {
				t.Fatalf("source peer error: %v", err)
			}
			stream, err := c.WatchAnswer(ctx, &assistantv1.WatchAnswerRequest{AnswerId: apiDialogueAnswer})
			if err == nil {
				_, err = stream.Recv()
			}
			if status.Code(err) != codes.Internal || strings.Contains(err.Error(), "private") {
				t.Fatalf("source stream peer error: %v", err)
			}
		})
	}
}

func TestDialogueAPIWatchCommittedFailureDiscardsPartial(t *testing.T) {
	env := apitest.Start(t)
	seedAPIDialogue(t, env, "generating")
	c := assistantv1.NewAssistantServiceClient(env.Conn)
	ctx, cancel := apiDialogueContext(t, apiDialogueUser)
	defer cancel()
	stream, err := c.WatchAnswer(ctx, &assistantv1.WatchAnswerRequest{AnswerId: apiDialogueAnswer})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatal(err)
	}
	err = env.App.DB.Run(ctx, func(ctx context.Context) error {
		if _, err := env.App.DB.Writer(ctx).Exec(ctx, `UPDATE answer_block SET markdown='saved but never published' WHERE user_id=$1 AND answer_id=$2`, apiDialogueUser, apiDialogueAnswer); err != nil {
			return err
		}
		if _, err := env.App.DB.Writer(ctx).Exec(ctx, `UPDATE answer SET status='failed',error_code='MODEL_ERROR',retryable=true,charged=false WHERE user_id=$1 AND id=$2`, apiDialogueUser, apiDialogueAnswer); err != nil {
			return err
		}
		_, err := env.App.DB.Writer(ctx).Exec(ctx, `SELECT pg_notify('assistant_answers',$1)`, apiDialogueAnswer)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	response, err := stream.Recv()
	if err != nil || response.GetError() == nil || !response.GetError().GetRetryable() {
		t.Fatalf("failed update must replace partial content: %v %v", response, err)
	}
	if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("failure EOF: %v", err)
	}
}
