package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	assistantv1 "github.com/fin-assist/contracts/gen/go/finassist/assistant/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	statementv1 "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	"github.com/fin-assist/contracts/gen/go/finerr"

	"github.com/fin-assist/assistant/internal/app"
	"github.com/fin-assist/assistant/internal/app/apitest"
	"github.com/fin-assist/assistant/internal/config"
)

const generationAPIUser = "10000000-0000-4000-8000-000000000001"
const generationAPIOther = "10000000-0000-4000-8000-000000000002"

var generationAPINow = time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)

type generationAPICall struct {
	method  string
	request proto.Message
	md      metadata.MD
}
type generationAPIPeers struct {
	userv1.UnimplementedUserInternalServiceServer
	statementv1.UnimplementedAssistantToolsServiceServer
	analyticsv1.UnimplementedAnalyticsServiceServer
	mu                sync.Mutex
	calls             []generationAPICall
	grant             bool
	consentError      bool
	consentHook       func(context.Context) error
	overviewResponse  *statementv1.GetDataOverviewResponse
	aggregateResponse *statementv1.AggregateOperationsResponse
	findResponse      *statementv1.FindOperationsResponse
	analyticsResponse *analyticsv1.GetAnalyticsResponse
}

func (p *generationAPIPeers) record(ctx context.Context, method string, req proto.Message) {
	md, _ := metadata.FromIncomingContext(ctx)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, generationAPICall{method, proto.Clone(req), md.Copy()})
}
func (p *generationAPIPeers) consent(grant, unavailable bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.grant = grant
	p.consentError = unavailable
}
func (p *generationAPIPeers) count(method string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, c := range p.calls {
		if c.method == method {
			n++
		}
	}
	return n
}
func (p *generationAPIPeers) GetAssistantConsentStatus(ctx context.Context, req *userv1.GetAssistantConsentStatusRequest) (*userv1.GetAssistantConsentStatusResponse, error) {
	p.record(ctx, "consent", req)
	p.mu.Lock()
	grant, unavailable, hook := p.grant, p.consentError, p.consentHook
	p.mu.Unlock()
	if hook != nil {
		if err := hook(ctx); err != nil {
			return nil, err
		}
	}
	if unavailable {
		return nil, status.Error(codes.Unavailable, "private consent dependency text")
	}
	version := "v1"
	return &userv1.GetAssistantConsentStatusResponse{Consent: &userv1.AssistantConsent{Granted: grant, Version: &version, CurrentVersion: version}}, nil
}
func generationAPIRange() *commonv1.TimeRange {
	return &commonv1.TimeRange{Start: timestamppb.New(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)), End: timestamppb.New(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))}
}
func generationAPIBounds() *commonv1.DataBounds {
	r := generationAPIRange()
	return &commonv1.DataBounds{Coverage: commonv1.Coverage_COVERAGE_COMPLETE, DataFrom: r.Start, DataTo: timestamppb.New(r.End.AsTime().Add(-time.Second))}
}
func (p *generationAPIPeers) GetDataOverview(ctx context.Context, req *statementv1.GetDataOverviewRequest) (*statementv1.GetDataOverviewResponse, error) {
	p.record(ctx, "overview", req)
	if p.overviewResponse != nil {
		return proto.Clone(p.overviewResponse).(*statementv1.GetDataOverviewResponse), nil
	}
	r := generationAPIRange()
	return &statementv1.GetDataOverviewResponse{HasData: true, DataFrom: r.Start, DataTo: r.End, FullMonths: 1, Months: []*statementv1.MonthCoverage{{Month: "2026-09", Range: r, Bounds: generationAPIBounds()}}, SuppliedFields: []string{"occurred_at", "amount_minor"}, SourceNames: []string{"test statement"}, Version: &statementv1.DataVersion{}, TimeZone: "Europe/Moscow", CalculatedAt: timestamppb.New(generationAPINow)}, nil
}
func (p *generationAPIPeers) GetDataVersion(ctx context.Context, req *statementv1.GetDataVersionRequest) (*statementv1.GetDataVersionResponse, error) {
	p.record(ctx, "version", req)
	return &statementv1.GetDataVersionResponse{Version: &statementv1.DataVersion{}}, nil
}
func (p *generationAPIPeers) AggregateOperations(ctx context.Context, req *statementv1.AggregateOperationsRequest) (*statementv1.AggregateOperationsResponse, error) {
	p.record(ctx, "aggregate", req)
	if p.aggregateResponse != nil {
		return proto.Clone(p.aggregateResponse).(*statementv1.AggregateOperationsResponse), nil
	}
	return &statementv1.AggregateOperationsResponse{TotalAmountMinor: 12345, TotalOperationCount: 1, Bounds: generationAPIBounds(), Range: generationAPIRange(), TimeZone: "Europe/Moscow", CalculatedAt: timestamppb.New(generationAPINow), Version: &statementv1.DataVersion{}}, nil
}
func (p *generationAPIPeers) FindOperations(ctx context.Context, req *statementv1.FindOperationsRequest) (*statementv1.FindOperationsResponse, error) {
	p.record(ctx, "find", req)
	if p.findResponse != nil {
		return proto.Clone(p.findResponse).(*statementv1.FindOperationsResponse), nil
	}
	return &statementv1.FindOperationsResponse{Bounds: generationAPIBounds(), Range: generationAPIRange(), TimeZone: "Europe/Moscow", Version: &statementv1.DataVersion{}}, nil
}
func (p *generationAPIPeers) GetAnalytics(ctx context.Context, req *analyticsv1.GetAnalyticsRequest) (*analyticsv1.GetAnalyticsResponse, error) {
	p.record(ctx, "analytics", req)
	if p.analyticsResponse != nil {
		return proto.Clone(p.analyticsResponse).(*analyticsv1.GetAnalyticsResponse), nil
	}
	return &analyticsv1.GetAnalyticsResponse{HasData: false}, nil
}
func (p *generationAPIPeers) ListPeriods(ctx context.Context, req *analyticsv1.ListPeriodsRequest) (*analyticsv1.ListPeriodsResponse, error) {
	p.record(ctx, "periods", req)
	return &analyticsv1.ListPeriodsResponse{TimeZone: "Europe/Moscow"}, nil
}

// One fake gRPC server represents three independent named downstream transports.
func generationAPIPeerOptions(t *testing.T, p *generationAPIPeers) map[string][]grpc.DialOption {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	userv1.RegisterUserInternalServiceServer(server, p)
	statementv1.RegisterAssistantToolsServiceServer(server, p)
	analyticsv1.RegisterAnalyticsServiceServer(server, p)
	go func() { _ = server.Serve(lis) }()
	t.Cleanup(server.Stop)
	options := map[string][]grpc.DialOption{}
	for _, peer := range []string{"user", "bank-statement-data", "analytics"} {
		options[peer] = []grpc.DialOption{grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) })}
	}
	return options
}

type generationAPIRequest struct {
	body   map[string]json.RawMessage
	header http.Header
}
type generationAPIHTTP struct {
	mu       sync.Mutex
	requests []generationAPIRequest
	count    atomic.Int32
	arrivals chan struct{}
	respond  func(int, http.ResponseWriter, *http.Request)
	server   *httptest.Server
}

func generationAPIModel(t *testing.T, respond func(int, http.ResponseWriter, *http.Request)) *generationAPIHTTP {
	t.Helper()
	m := &generationAPIHTTP{arrivals: make(chan struct{}, 100), respond: respond}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { _ = r.Body.Close() }()
		if r.Method != "POST" || r.URL.Path != "/v1/responses" {
			t.Errorf("unexpected model destination: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(400)
			return
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("model input: %v", err)
			w.WriteHeader(400)
			return
		}
		m.mu.Lock()
		m.requests = append(m.requests, generationAPIRequest{body, r.Header.Clone()})
		m.mu.Unlock()
		n := int(m.count.Add(1))
		m.arrivals <- struct{}{}
		w.Header().Set("Content-Type", "text/event-stream")
		if m.respond != nil {
			m.respond(n, w, r)
		} else {
			_, _ = io.WriteString(w, generationAPIText("Финансовый ответ"))
		}
	}))
	t.Cleanup(m.server.Close)
	return m
}
func generationAPIFrames(events ...map[string]any) string {
	var b strings.Builder
	for _, event := range events {
		raw, err := json.Marshal(event)
		if err != nil {
			panic(err)
		}
		b.WriteString("data: ")
		b.Write(raw)
		b.WriteString("\n\n")
	}
	return b.String()
}
func generationAPICreated() map[string]any {
	return map[string]any{"type": "response.created", "response": map[string]any{"id": "r", "status": "in_progress", "output": []any{}}}
}
func generationAPICompleted(item any) map[string]any {
	return map[string]any{"type": "response.completed", "response": map[string]any{"id": "r", "status": "completed", "output": []any{item}, "usage": map[string]any{"input_tokens": 10, "output_tokens": 4, "total_tokens": 14, "tool_tokens": 0}}}
}
func generationAPIText(text string) string {
	part := map[string]any{"type": "output_text", "text": text}
	item := map[string]any{"id": "m", "type": "message", "role": "assistant", "status": "completed", "content": []any{part}}
	return generationAPIFrames(generationAPICreated(), map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"id": "m", "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}}}, map[string]any{"type": "response.content_part.added", "item_id": "m", "output_index": 0, "content_index": 0, "part": map[string]any{"type": "output_text", "text": ""}}, map[string]any{"type": "response.output_text.delta", "item_id": "m", "output_index": 0, "content_index": 0, "delta": text}, map[string]any{"type": "response.output_text.done", "item_id": "m", "output_index": 0, "content_index": 0, "text": text}, map[string]any{"type": "response.content_part.done", "item_id": "m", "output_index": 0, "content_index": 0, "part": part}, map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item}, generationAPICompleted(item))
}
func generationAPIFunction(name, args string) string {
	return generationAPIFunctionID("c", name, args)
}
func generationAPIFunctionID(id, name, args string) string {
	item := map[string]any{"id": id, "type": "function_call", "call_id": id, "name": name, "arguments": args, "status": "completed"}
	return generationAPIFrames(generationAPICreated(), map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"id": id, "type": "function_call", "call_id": id, "name": name, "arguments": "", "status": "in_progress"}}, map[string]any{"type": "response.function_call_arguments.delta", "output_index": 0, "item_id": id, "delta": args}, map[string]any{"type": "response.function_call_arguments.done", "output_index": 0, "item_id": id, "name": name, "arguments": args}, map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item}, generationAPICompleted(item))
}
func generationAPIEnv(t *testing.T, p *generationAPIPeers, m *generationAPIHTTP, mutate ...func(*config.Config)) *apitest.Env {
	t.Helper()
	key := filepath.Join(t.TempDir(), "model-key")
	if err := os.WriteFile(key, []byte("local-test-key"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := func(c *config.Config) {
		for _, peer := range []string{"user", "bank-statement-data", "analytics"} {
			c.Peers[peer] = "passthrough:///" + peer
		}
		c.Generation = config.GenerationConfig{ModelURI: "gpt://folder/aliceai-llm", FolderID: "folder", Endpoint: m.server.URL + "/v1/responses", APIKeyFile: key, InputTokensPerStep: 24000, InputTokensTotal: 48000, HistoryTokens: 16000, RequestBytesPerStep: 65536, RequestBytesTotal: 131072, MaxOutputTokens: 4096, MaxSteps: 8, MaxFunctions: 16, ParallelFunctions: 4, MaxSearchCalls: 3, FinancialResultBytes: 32768, FinancialCollectionItems: 200, ModelResponseBytes: 1048576, ModelEventBytes: 65536, ModelArgumentBytes: 16384, ModelOutputItems: 64}
	}
	return apitest.StartWithOptions(t, app.Options{Clock: func() time.Time { return generationAPINow }, PeerDialOptions: generationAPIPeerOptions(t, p)}, append([]func(*config.Config){cfg}, mutate...)...)
}
func generationAPIContext(t *testing.T, user, key string) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	t.Cleanup(cancel)
	ctx = apitest.User(ctx, user)
	if key != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "idempotency-key", key)
	}
	return ctx
}
func generationAPIKey() string { return uuid.NewString() }
func generationAPIAsk(t *testing.T, env *apitest.Env, user, key, text string, mode commonv1.TransferMode) *assistantv1.AskQuestionResponse {
	t.Helper()
	got, err := assistantv1.NewAssistantServiceClient(env.Conn).AskQuestion(generationAPIContext(t, user, key), &assistantv1.AskQuestionRequest{Text: text, TransferMode: mode})
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	return got
}
func generationAPITerminal(t *testing.T, env *apitest.Env, user, answer string) (*assistantv1.AnswerDone, *assistantv1.AnswerError) {
	t.Helper()
	stream, err := assistantv1.NewAssistantServiceClient(env.Conn).WatchAnswer(generationAPIContext(t, user, ""), &assistantv1.WatchAnswerRequest{AnswerId: answer})
	if err != nil {
		t.Fatal(err)
	}
	first := true
	for {
		event, err := stream.Recv()
		if err != nil {
			t.Fatalf("answer transport before terminal: %v", err)
		}
		if first && event.GetSnapshot() == nil {
			t.Fatal("first response is not a snapshot")
		}
		first = false
		if event.GetDone() != nil || event.GetError() != nil {
			if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
				t.Fatalf("terminal EOF: %v", err)
			}
			return event.GetDone(), event.GetError()
		}
	}
}
func generationAPICount(t *testing.T, env *apitest.Env, table string) int {
	t.Helper()
	queries := map[string]string{"question": "SELECT count(*) FROM question", "answer": "SELECT count(*) FROM answer", "key": "SELECT count(*) FROM idempotency_keys"}
	query, ok := queries[table]
	if !ok {
		t.Fatal("unknown table")
	}
	var n int
	if err := env.App.DB.Writer(t.Context()).QueryRow(t.Context(), query).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
func generationAPIAssertError(t *testing.T, err error, code codes.Code, reason string) {
	t.Helper()
	if status.Code(err) != code || apitest.Reason(err) != reason {
		t.Fatalf("got %v (%s), want %s %s", err, apitest.Reason(err), code, reason)
	}
	if reason != "" {
		rest, ok := finerr.ToREST(err)
		if !ok {
			t.Fatalf("incomplete contract details: %v", err)
		}
		if reason == "LIMIT_EXCEEDED" && rest.Metadata["resets_at"] != "2026-10-24T00:00:00+03:00" {
			t.Fatalf("reset metadata: %+v", rest)
		}
		if reason == "REQUEST_IN_PROGRESS" && rest.RetryAfter <= 0 {
			t.Fatal("reservation refusal omitted positive RetryInfo")
		}
	}
	if strings.Contains(fmt.Sprint(err), "private") {
		t.Fatal("dependency detail leaked")
	}
}
func generationAPIWaitArrival(t *testing.T, m *generationAPIHTTP) {
	t.Helper()
	select {
	case <-m.arrivals:
	case <-time.After(5 * time.Second):
		t.Fatal("model request did not arrive")
	}
}

func TestGenerationAPIAskWatchHistoryAndWirePolicy(t *testing.T) {
	p := &generationAPIPeers{grant: true}
	m := generationAPIModel(t, nil)
	env := generationAPIEnv(t, p, m)
	c := assistantv1.NewAssistantServiceClient(env.Conn)
	accepted := generationAPIAsk(t, env, generationAPIUser, generationAPIKey(), "Покажи расходы", commonv1.TransferMode_TRANSFER_MODE_WITHOUT)
	if accepted.Question.GetText() != "Покажи расходы" || accepted.Question.GetRole() != assistantv1.Role_ROLE_USER || accepted.Question.Currency != "RUB" || accepted.Question.Id == "" || accepted.AnswerId == "" || accepted.RemainingLimit != 4 || !accepted.Question.CreatedAt.AsTime().Equal(generationAPINow) {
		t.Fatal(accepted)
	}
	done, failed := generationAPITerminal(t, env, generationAPIUser, accepted.AnswerId)
	if failed != nil || done == nil || !done.Charged || done.RemainingLimit != 4 {
		t.Fatalf("done=%v failed=%v", done, failed)
	}
	page, err := c.ListMessages(generationAPIContext(t, generationAPIUser, ""), &assistantv1.ListMessagesRequest{})
	if err != nil || len(page.GetMessages()) != 2 {
		t.Fatalf("history=%v %v", page, err)
	}
	answer := page.Messages[1]
	if answer.Id != accepted.AnswerId || answer.GetStatus() != assistantv1.AnswerStatus_ANSWER_STATUS_COMPLETE || len(answer.Blocks) != 1 || answer.Blocks[0].GetText().GetText() != "Финансовый ответ" {
		t.Fatal(page)
	}
	if m.count.Load() != 1 || generationAPICount(t, env, "question") != 1 || generationAPICount(t, env, "answer") != 1 || generationAPICount(t, env, "key") != 1 {
		t.Fatal("acceptance or generation duplicated")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	request := m.requests[0]
	for header, want := range map[string]string{"Authorization": "Bearer local-test-key", "OpenAI-Project": "folder", "X-Data-Logging-Enabled": "false"} {
		if request.header.Get(header) != want {
			t.Errorf("header%s=%q", header, request.header.Get(header))
		}
	}
	for field, want := range map[string]string{"model": "\"gpt://folder/aliceai-llm\"", "store": "false", "stream": "true", "truncation": "\"disabled\""} {
		if string(request.body[field]) != want {
			t.Errorf("wire%s=%s", field, request.body[field])
		}
	}
	if len(request.body["instructions"]) == 0 || len(request.body["tools"]) == 0 || len(request.body["previous_response_id"]) != 0 || len(request.body["conversation"]) != 0 {
		t.Fatal("context policy")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, call := range p.calls {
		if len(call.md.Get("authorization")) != 0 || len(call.md.Get("x-service-authorization")) != 1 || call.md.Get("x-service-authorization")[0] != "Bearer dev:apitest" {
			t.Fatalf("peer metadata: %s %v", call.method, call.md)
		}
		switch req := call.request.(type) {
		case *userv1.GetAssistantConsentStatusRequest:
			if req.GetUserId() != generationAPIUser {
				t.Fatal(req)
			}
		case *statementv1.GetDataOverviewRequest:
			if req.UserId != generationAPIUser {
				t.Fatal(req)
			}
		}
	}
}
func TestGenerationAPIConsentRefusalsReleaseKeyAndAllowSameKeyLater(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		t.Run(fmt.Sprint(unavailable), func(t *testing.T) {
			p := &generationAPIPeers{consentError: unavailable}
			m := generationAPIModel(t, nil)
			env := generationAPIEnv(t, p, m)
			c := assistantv1.NewAssistantServiceClient(env.Conn)
			key := generationAPIKey()
			req := &assistantv1.AskQuestionRequest{Text: "Финансы"}
			_, err := c.AskQuestion(generationAPIContext(t, generationAPIUser, key), req)
			if unavailable {
				generationAPIAssertError(t, err, codes.Unavailable, "")
			} else {
				generationAPIAssertError(t, err, codes.PermissionDenied, "CONSENT_REQUIRED")
			}
			if generationAPICount(t, env, "question") != 0 || generationAPICount(t, env, "answer") != 0 || generationAPICount(t, env, "key") != 0 || m.count.Load() != 0 {
				t.Fatal("denied acceptance left side effects")
			}
			limit, err := c.GetLimit(generationAPIContext(t, generationAPIUser, ""), &assistantv1.GetLimitRequest{})
			if err != nil || limit.Remaining != 5 {
				t.Fatalf("%v %v", limit, err)
			}
			p.consent(true, false)
			accepted, err := c.AskQuestion(generationAPIContext(t, generationAPIUser, key), req)
			if err != nil {
				t.Fatal(err)
			}
			done, failed := generationAPITerminal(t, env, generationAPIUser, accepted.AnswerId)
			if done == nil || failed != nil || m.count.Load() != 1 {
				t.Fatalf("%v %v", done, failed)
			}
		})
	}
}
func TestGenerationAPIReplayPrecedesConsentAndDailyLimit(t *testing.T) {
	p := &generationAPIPeers{grant: true}
	m := generationAPIModel(t, nil)
	env := generationAPIEnv(t, p, m, func(c *config.Config) { c.DailyMax = 1 })
	c := assistantv1.NewAssistantServiceClient(env.Conn)
	key := generationAPIKey()
	req := &assistantv1.AskQuestionRequest{Text: "Мои расходы"}
	first, err := c.AskQuestion(generationAPIContext(t, generationAPIUser, key), req)
	if err != nil {
		t.Fatal(err)
	}
	generationAPITerminal(t, env, generationAPIUser, first.AnswerId)
	p.consent(false, false)
	consents := p.count("consent")
	again, err := c.AskQuestion(generationAPIContext(t, generationAPIUser, key), req)
	if err != nil || !proto.Equal(first, again) || p.count("consent") != consents || m.count.Load() != 1 {
		t.Fatalf("replay=%v %v", again, err)
	}
	_, err = c.AskQuestion(generationAPIContext(t, generationAPIUser, key), &assistantv1.AskQuestionRequest{Text: "Другой вопрос"})
	generationAPIAssertError(t, err, codes.AlreadyExists, "IDEMPOTENCY_CONFLICT")
	p.consent(true, false)
	_, err = c.AskQuestion(generationAPIContext(t, generationAPIUser, generationAPIKey()), req)
	generationAPIAssertError(t, err, codes.ResourceExhausted, "LIMIT_EXCEEDED")
	if m.count.Load() != 1 || generationAPICount(t, env, "question") != 1 {
		t.Fatal("limit created effect")
	}
}
func TestGenerationAPIConcurrentDuplicateAndDailyLimit(t *testing.T) {
	t.Run("reserved while consent pending", func(t *testing.T) {
		entered, resume := make(chan struct{}), make(chan struct{})
		var once sync.Once
		p := &generationAPIPeers{grant: true, consentHook: func(ctx context.Context) error {
			once.Do(func() { close(entered) })
			select {
			case <-resume:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}}
		m := generationAPIModel(t, nil)
		env := generationAPIEnv(t, p, m)
		c := assistantv1.NewAssistantServiceClient(env.Conn)
		key := generationAPIKey()
		owner := make(chan *assistantv1.AskQuestionResponse, 1)
		ownerErr := make(chan error, 1)
		go func() {
			result, err := c.AskQuestion(generationAPIContext(t, generationAPIUser, key), &assistantv1.AskQuestionRequest{Text: "q"})
			owner <- result
			ownerErr <- err
		}()
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("consent did not start")
		}
		_, err := c.AskQuestion(generationAPIContext(t, generationAPIUser, key), &assistantv1.AskQuestionRequest{Text: "q"})
		generationAPIAssertError(t, err, codes.Aborted, "REQUEST_IN_PROGRESS")
		if m.count.Load() != 0 || p.count("consent") != 1 {
			t.Fatal("duplicate reached work")
		}
		close(resume)
		accepted := <-owner
		if err := <-ownerErr; err != nil {
			t.Fatal(err)
		}
		generationAPITerminal(t, env, generationAPIUser, accepted.AnswerId)
		if m.count.Load() != 1 || generationAPICount(t, env, "question") != 1 {
			t.Fatal("duplicate work")
		}
	})
	t.Run("distinct keys serialize daily charge", func(t *testing.T) {
		p := &generationAPIPeers{grant: true}
		m := generationAPIModel(t, nil)
		env := generationAPIEnv(t, p, m, func(c *config.Config) { c.DailyMax = 1 })
		c := assistantv1.NewAssistantServiceClient(env.Conn)
		start := make(chan struct{})
		type result struct {
			response *assistantv1.AskQuestionResponse
			err      error
		}
		results := make(chan result, 2)
		for range 2 {
			go func() {
				<-start
				r, e := c.AskQuestion(generationAPIContext(t, generationAPIUser, generationAPIKey()), &assistantv1.AskQuestionRequest{Text: "q"})
				results <- result{r, e}
			}()
		}
		close(start)
		accepted, denied := 0, 0
		for range 2 {
			r := <-results
			if r.err == nil {
				accepted++
				generationAPITerminal(t, env, generationAPIUser, r.response.AnswerId)
			} else {
				denied++
				generationAPIAssertError(t, r.err, codes.ResourceExhausted, "LIMIT_EXCEEDED")
			}
		}
		if accepted != 1 || denied != 1 || generationAPICount(t, env, "question") != 1 || m.count.Load() != 1 {
			t.Fatalf("accepted%d denied%d", accepted, denied)
		}
	})
}

func generationAPISeedAnswer(t *testing.T, env *apitest.Env, state, code string, retryable *bool) (string, string) {
	t.Helper()
	question, answer := generationAPIKey(), generationAPIKey()
	charged := state == "generating"
	used := 0
	if charged {
		used = 1
	}
	err := env.App.DB.Run(t.Context(), func(ctx context.Context) error {
		if _, err := env.App.DB.Writer(ctx).Exec(ctx, `INSERT INTO user_state(user_id,limit_day,limit_used,time_zone) VALUES ($1,'2026-10-23',$2,'Europe/Moscow')`, generationAPIUser, used); err != nil {
			return err
		}
		if _, err := env.App.DB.Writer(ctx).Exec(ctx, `INSERT INTO question(user_id,id,text,transfer_mode,created_at) VALUES ($1,$2,'saved original question','without',$3)`, generationAPIUser, question, generationAPINow); err != nil {
			return err
		}
		var errorCode *string
		if code != "" {
			errorCode = &code
		}
		_, err := env.App.DB.Writer(ctx).Exec(ctx, `INSERT INTO answer(user_id,id,question_id,status,error_code,retryable,charge_day,charged,created_at) VALUES ($1,$2,$3,$4,$5,$6,'2026-10-23',$7,$8)`, generationAPIUser, answer, question, state, errorCode, retryable, charged, generationAPINow)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return question, answer
}
func TestGenerationAPIRetryFailedAnswerPreservesQuestionAndReplays(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprint(legacy), func(t *testing.T) {
			p := &generationAPIPeers{grant: true}
			m := generationAPIModel(t, nil)
			env := generationAPIEnv(t, p, m)
			retryable := true
			var policy *bool
			if !legacy {
				policy = &retryable
			}
			question, old := generationAPISeedAnswer(t, env, "failed", "MODEL_ERROR", policy)
			c := assistantv1.NewAssistantServiceClient(env.Conn)
			key := generationAPIKey()
			req := &assistantv1.RetryAnswerRequest{AnswerId: old}
			got, err := c.RetryAnswer(generationAPIContext(t, generationAPIUser, key), req)
			if err != nil || got.AnswerId == old || got.RemainingLimit != 4 {
				t.Fatalf("%v %v", got, err)
			}
			done, failed := generationAPITerminal(t, env, generationAPIUser, got.AnswerId)
			if done == nil || !done.Charged || failed != nil {
				t.Fatalf("%v %v", done, failed)
			}
			stream, err := c.WatchAnswer(generationAPIContext(t, generationAPIUser, ""), &assistantv1.WatchAnswerRequest{AnswerId: old})
			if err == nil {
				_, err = stream.Recv()
			}
			generationAPIAssertError(t, err, codes.NotFound, "NOT_FOUND")
			page, err := c.ListMessages(generationAPIContext(t, generationAPIUser, ""), &assistantv1.ListMessagesRequest{})
			if err != nil || len(page.Messages) != 2 || page.Messages[0].Id != question || page.Messages[1].Id != got.AnswerId {
				t.Fatalf("%v %v", page, err)
			}
			var mode, text string
			if err := env.App.DB.Writer(t.Context()).QueryRow(t.Context(), `SELECT q.transfer_mode,q.text FROM question q JOIN answer a ON a.user_id=q.user_id AND a.question_id=q.id WHERE a.id=$1`, got.AnswerId).Scan(&mode, &text); err != nil || mode != "without" || text != "saved original question" {
				t.Fatalf("mode%s text%s %v", mode, text, err)
			}
			p.consent(false, false)
			calls := p.count("consent")
			again, err := c.RetryAnswer(generationAPIContext(t, generationAPIUser, key), req)
			if err != nil || !proto.Equal(got, again) || p.count("consent") != calls || m.count.Load() != 1 || generationAPICount(t, env, "question") != 1 || generationAPICount(t, env, "answer") != 2 {
				t.Fatalf("replay%v %v", again, err)
			}
		})
	}
}
func TestGenerationAPIRetryRefusalsRollbackChargeAndDoNotCache(t *testing.T) {
	t.Run("generating later fails", func(t *testing.T) {
		p := &generationAPIPeers{grant: true}
		m := generationAPIModel(t, nil)
		env := generationAPIEnv(t, p, m)
		_, old := generationAPISeedAnswer(t, env, "generating", "", nil)
		c := assistantv1.NewAssistantServiceClient(env.Conn)
		key := generationAPIKey()
		req := &assistantv1.RetryAnswerRequest{AnswerId: old}
		_, err := c.RetryAnswer(generationAPIContext(t, generationAPIUser, key), req)
		generationAPIAssertError(t, err, codes.FailedPrecondition, "RETRY_NOT_ALLOWED")
		if generationAPICount(t, env, "key") != 0 || m.count.Load() != 0 {
			t.Fatal("refusal cached or called model")
		}
		limit, err := c.GetLimit(generationAPIContext(t, generationAPIUser, ""), &assistantv1.GetLimitRequest{})
		if err != nil || limit.Remaining != 4 {
			t.Fatalf("charge not rolled back %v %v", limit, err)
		}
		err = env.App.DB.Run(t.Context(), func(ctx context.Context) error {
			if _, err := env.App.DB.Writer(ctx).Exec(ctx, `UPDATE answer SET status='failed',error_code='MODEL_ERROR',retryable=true,charged=false WHERE id=$1`, old); err != nil {
				return err
			}
			_, err := env.App.DB.Writer(ctx).Exec(ctx, `UPDATE user_state SET limit_used=0 WHERE user_id=$1`, generationAPIUser)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		got, err := c.RetryAnswer(generationAPIContext(t, generationAPIUser, key), req)
		if err != nil {
			t.Fatal(err)
		}
		done, failed := generationAPITerminal(t, env, generationAPIUser, got.AnswerId)
		if done == nil || failed != nil || m.count.Load() != 1 {
			t.Fatalf("%v %v", done, failed)
		}
	})
	for _, test := range []string{"explicit false", "foreign", "consent", "daily before missing"} {
		t.Run(test, func(t *testing.T) {
			p := &generationAPIPeers{grant: true}
			m := generationAPIModel(t, nil)
			env := generationAPIEnv(t, p, m)
			policy := false
			_, old := generationAPISeedAnswer(t, env, "failed", "MODEL_ERROR", &policy)
			user, target := generationAPIUser, old
			wantCode, wantReason := codes.FailedPrecondition, "RETRY_NOT_ALLOWED"
			switch test {
			case "foreign":
				user = generationAPIOther
				wantCode, wantReason = codes.NotFound, "NOT_FOUND"
			case "consent":
				p.consent(false, false)
				wantCode, wantReason = codes.PermissionDenied, "CONSENT_REQUIRED"
			case "daily before missing":
				if _, err := env.App.DB.Writer(t.Context()).Exec(t.Context(), `UPDATE user_state SET limit_used=5 WHERE user_id=$1`, generationAPIUser); err != nil {
					t.Fatal(err)
				}
				target = generationAPIKey()
				wantCode, wantReason = codes.ResourceExhausted, "LIMIT_EXCEEDED"
			}
			_, err := assistantv1.NewAssistantServiceClient(env.Conn).RetryAnswer(generationAPIContext(t, user, generationAPIKey()), &assistantv1.RetryAnswerRequest{AnswerId: target})
			generationAPIAssertError(t, err, wantCode, wantReason)
			if m.count.Load() != 0 || generationAPICount(t, env, "key") != 0 || generationAPICount(t, env, "answer") != 1 {
				t.Fatal("refused retry left effects")
			}
		})
	}
}
func TestGenerationAPIFailureDiscardsPartialRefundsAndCanRetry(t *testing.T) {
	p := &generationAPIPeers{grant: true}
	m := generationAPIModel(t, func(n int, w http.ResponseWriter, _ *http.Request) {
		if n == 1 {
			partial := generationAPIText("Этого текста не должно остаться")
			_, _ = io.WriteString(w, partial[:strings.LastIndex(partial, "data: ")])
			return
		}
		_, _ = io.WriteString(w, generationAPIText("Повтор успешен"))
	})
	env := generationAPIEnv(t, p, m)
	c := assistantv1.NewAssistantServiceClient(env.Conn)
	accepted := generationAPIAsk(t, env, generationAPIUser, generationAPIKey(), "Расходы", commonv1.TransferMode_TRANSFER_MODE_WITH)
	done, failed := generationAPITerminal(t, env, generationAPIUser, accepted.AnswerId)
	if done != nil || failed == nil || failed.Code != assistantv1.AnswerErrorCode_ANSWER_ERROR_CODE_MODEL_ERROR || !failed.Retryable || failed.RemainingLimit != 5 {
		t.Fatalf("%v %v", done, failed)
	}
	page, err := c.ListMessages(generationAPIContext(t, generationAPIUser, ""), &assistantv1.ListMessagesRequest{})
	if err != nil || len(page.Messages) != 2 || page.Messages[1].GetStatus() != assistantv1.AnswerStatus_ANSWER_STATUS_FAILED || len(page.Messages[1].Blocks) != 0 {
		t.Fatalf("partial retained: %v %v", page, err)
	}
	retried, err := c.RetryAnswer(generationAPIContext(t, generationAPIUser, generationAPIKey()), &assistantv1.RetryAnswerRequest{AnswerId: accepted.AnswerId})
	if err != nil {
		t.Fatal(err)
	}
	done, failed = generationAPITerminal(t, env, generationAPIUser, retried.AnswerId)
	if done == nil || !done.Charged || failed != nil || m.count.Load() != 2 {
		t.Fatalf("retry %v %v", done, failed)
	}
}
func TestGenerationAPICanceledWatchDoesNotCancelCommittedGeneration(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	p := &generationAPIPeers{grant: true}
	m := generationAPIModel(t, func(_ int, w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
			_, _ = io.WriteString(w, generationAPIText("Готово после отмены клиента"))
		case <-r.Context().Done():
		}
	})
	env := generationAPIEnv(t, p, m)
	c := assistantv1.NewAssistantServiceClient(env.Conn)
	ctx, cancel := context.WithCancel(generationAPIContext(t, generationAPIUser, generationAPIKey()))
	accepted, err := c.AskQuestion(ctx, &assistantv1.AskQuestionRequest{Text: "Финансы"})
	if err != nil {
		t.Fatal(err)
	}
	generationAPIWaitArrival(t, m)
	stream, err := c.WatchAnswer(ctx, &assistantv1.WatchAnswerRequest{AnswerId: accepted.AnswerId})
	if err != nil {
		t.Fatal(err)
	}
	if first, err := stream.Recv(); err != nil || first.GetSnapshot().GetStatus() != assistantv1.AnswerStatus_ANSWER_STATUS_GENERATING {
		t.Fatalf("%v %v", first, err)
	}
	cancel()
	if _, err := stream.Recv(); status.Code(err) != codes.Canceled {
		t.Fatal(err)
	}
	once.Do(func() { close(release) })
	done, failed := generationAPITerminal(t, env, generationAPIUser, accepted.AnswerId)
	if done == nil || !done.Charged || failed != nil || m.count.Load() != 1 {
		t.Fatalf("%v %v", done, failed)
	}
}
func TestGenerationAPINoDataToolEvidenceRefundsCompleteAnswer(t *testing.T) {
	p := &generationAPIPeers{grant: true}
	m := generationAPIModel(t, func(n int, w http.ResponseWriter, _ *http.Request) {
		if n == 1 {
			_, _ = io.WriteString(w, generationAPIFunction("get_analytics", `{"period":"month"}`))
			return
		}
		_, _ = io.WriteString(w, generationAPIText("Данных для аналитики пока нет"))
	})
	env := generationAPIEnv(t, p, m)
	accepted := generationAPIAsk(t, env, generationAPIUser, generationAPIKey(), "Аналитика", commonv1.TransferMode_TRANSFER_MODE_WITHOUT)
	done, failed := generationAPITerminal(t, env, generationAPIUser, accepted.AnswerId)
	if done == nil || done.Charged || done.RemainingLimit != 5 || failed != nil || m.count.Load() != 2 || p.count("analytics") != 1 {
		t.Fatalf("%v %v modelsteps%d analytics%d", done, failed, m.count.Load(), p.count("analytics"))
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, call := range p.calls {
		if req, ok := call.request.(*analyticsv1.GetAnalyticsRequest); ok {
			if req.GetUserId() != generationAPIUser || req.TransferMode != commonv1.TransferMode_TRANSFER_MODE_WITHOUT || len(call.md.Get("authorization")) != 0 {
				t.Fatalf("scope/mode%v %v", req, call.md)
			}
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.requests) != 2 || !strings.Contains(string(m.requests[1].body["input"]), "function_call_output") {
		t.Fatal("tool result missing from continuation")
	}
}
func TestGenerationAPIValidationAndCallerBoundaries(t *testing.T) {
	p := &generationAPIPeers{grant: true}
	m := generationAPIModel(t, nil)
	env := generationAPIEnv(t, p, m)
	c := assistantv1.NewAssistantServiceClient(env.Conn)
	wrong := metadata.NewOutgoingContext(t.Context(), metadata.Pairs("x-service-authorization", "Bearer dev:user", "authorization", "Bearer dev:"+generationAPIUser, "idempotency-key", generationAPIKey()))
	_, err := c.AskQuestion(wrong, &assistantv1.AskQuestionRequest{Text: "q"})
	generationAPIAssertError(t, err, codes.PermissionDenied, "")
	invalid := metadata.NewOutgoingContext(t.Context(), metadata.Pairs("x-service-authorization", "Bearer dev:bank-bff", "authorization", "Bearer invalid", "idempotency-key", generationAPIKey()))
	_, err = c.AskQuestion(invalid, &assistantv1.AskQuestionRequest{Text: "q"})
	generationAPIAssertError(t, err, codes.Unauthenticated, "INVALID_TOKEN")
	for _, ctx := range []context.Context{generationAPIContext(t, generationAPIUser, ""), metadata.AppendToOutgoingContext(generationAPIContext(t, generationAPIUser, generationAPIKey()), "idempotency-key", generationAPIKey())} {
		_, err = c.AskQuestion(ctx, &assistantv1.AskQuestionRequest{Text: "q"})
		generationAPIAssertError(t, err, codes.InvalidArgument, "VALIDATION_ERROR")
	}
	for _, req := range []*assistantv1.AskQuestionRequest{{Text: ""}, {Text: strings.Repeat("я", 2001)}, {Text: "q", TransferMode: commonv1.TransferMode(99)}} {
		_, err = c.AskQuestion(generationAPIContext(t, generationAPIUser, generationAPIKey()), req)
		generationAPIAssertError(t, err, codes.InvalidArgument, "VALIDATION_ERROR")
	}
	_, err = c.RetryAnswer(generationAPIContext(t, generationAPIUser, generationAPIKey()), &assistantv1.RetryAnswerRequest{AnswerId: "not uuid"})
	generationAPIAssertError(t, err, codes.InvalidArgument, "VALIDATION_ERROR")
	_, err = c.RetryAnswer(wrong, &assistantv1.RetryAnswerRequest{AnswerId: generationAPIKey()})
	generationAPIAssertError(t, err, codes.PermissionDenied, "")
	_, err = c.RetryAnswer(invalid, &assistantv1.RetryAnswerRequest{AnswerId: generationAPIKey()})
	generationAPIAssertError(t, err, codes.Unauthenticated, "INVALID_TOKEN")
	for _, ctx := range []context.Context{generationAPIContext(t, generationAPIUser, ""), metadata.AppendToOutgoingContext(generationAPIContext(t, generationAPIUser, generationAPIKey()), "idempotency-key", generationAPIKey())} {
		_, err = c.RetryAnswer(ctx, &assistantv1.RetryAnswerRequest{AnswerId: generationAPIKey()})
		generationAPIAssertError(t, err, codes.InvalidArgument, "VALIDATION_ERROR")
	}
	if p.count("consent") != 0 || m.count.Load() != 0 || generationAPICount(t, env, "question") != 0 || generationAPICount(t, env, "key") != 0 {
		t.Fatal("invalid input crossed effects boundary")
	}
}

func TestGenerationAPIRequiredAggregateSourceAndScope(t *testing.T) {
	p := &generationAPIPeers{grant: true}
	m := generationAPIModel(t, func(n int, w http.ResponseWriter, _ *http.Request) {
		if n == 1 {
			_, _ = io.WriteString(w, generationAPIFunction("aggregate_operations", `{"all_time":true,"group_by":[]}`))
			return
		}
		_, _ = io.WriteString(w, generationAPIText("Расходы посчитаны по выписке"))
	})
	env := generationAPIEnv(t, p, m)
	accepted := generationAPIAsk(t, env, generationAPIUser, generationAPIKey(), "Итог расходов", commonv1.TransferMode_TRANSFER_MODE_WITHOUT)
	done, failed := generationAPITerminal(t, env, generationAPIUser, accepted.AnswerId)
	if done == nil || !done.Charged || failed != nil || p.count("aggregate") != 1 || m.count.Load() != 2 {
		t.Fatalf("%v %v", done, failed)
	}
	page, err := assistantv1.NewAssistantServiceClient(env.Conn).ListMessages(generationAPIContext(t, generationAPIUser, ""), &assistantv1.ListMessagesRequest{})
	if err != nil || len(page.Messages) != 2 {
		t.Fatalf("%v %v", page, err)
	}
	source := page.Messages[1].GetSource()
	if source == nil || !source.CalculatedAt.AsTime().Equal(generationAPINow) || source.TransferMode != commonv1.TransferMode_TRANSFER_MODE_WITHOUT || source.DataRange == nil {
		t.Fatal(source)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, call := range p.calls {
		if req, ok := call.request.(*statementv1.AggregateOperationsRequest); ok {
			if req.UserId != generationAPIUser || req.Filter.GetTransferMode() != commonv1.TransferMode_TRANSFER_MODE_WITHOUT || !req.AllTime || len(call.md.Get("authorization")) != 0 || len(call.md.Get("x-service-authorization")) != 1 || call.md.Get("x-service-authorization")[0] != "Bearer dev:apitest" {
				t.Fatalf("%v %v", req, call.md)
			}
		}
	}
}
func TestGenerationAPICannotAnswerIsFreeAndNotRetryable(t *testing.T) {
	p := &generationAPIPeers{grant: true}
	m := generationAPIModel(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, generationAPIFunction("cannot_answer", `{}`))
	})
	env := generationAPIEnv(t, p, m)
	accepted := generationAPIAsk(t, env, generationAPIUser, generationAPIKey(), "Непонятный вопрос", commonv1.TransferMode_TRANSFER_MODE_WITH)
	done, failed := generationAPITerminal(t, env, generationAPIUser, accepted.AnswerId)
	if done != nil || failed == nil || failed.Code != assistantv1.AnswerErrorCode_ANSWER_ERROR_CODE_CANNOT_ANSWER || failed.Retryable || failed.RemainingLimit != 5 || failed.Text == "" || m.count.Load() != 1 {
		t.Fatalf("%v %v", done, failed)
	}
	_, err := assistantv1.NewAssistantServiceClient(env.Conn).RetryAnswer(generationAPIContext(t, generationAPIUser, generationAPIKey()), &assistantv1.RetryAnswerRequest{AnswerId: accepted.AnswerId})
	generationAPIAssertError(t, err, codes.FailedPrecondition, "RETRY_NOT_ALLOWED")
	if m.count.Load() != 1 {
		t.Fatal("nonretryable answer called model")
	}
}

func TestGenerationAPIHistoryUsesOnlyCompletePairs(t *testing.T) {
	p := &generationAPIPeers{grant: true}
	m := generationAPIModel(t, nil)
	env := generationAPIEnv(t, p, m)
	first := generationAPIAsk(t, env, generationAPIUser, generationAPIKey(), "Первый финансовый вопрос", commonv1.TransferMode_TRANSFER_MODE_WITH)
	generationAPITerminal(t, env, generationAPIUser, first.AnswerId)
	err := env.App.DB.Run(t.Context(), func(ctx context.Context) error {
		var q string
		if err := env.App.DB.Writer(ctx).QueryRow(ctx, `INSERT INTO question(user_id,text,transfer_mode,created_at) VALUES ($1,'FAILED_QUESTION_MUST_NOT_ENTER_HISTORY','with',$2) RETURNING id::text`, generationAPIUser, generationAPINow).Scan(&q); err != nil {
			return err
		}
		_, err := env.App.DB.Writer(ctx).Exec(ctx, `INSERT INTO answer(user_id,question_id,status,error_code,retryable,charge_day,charged,created_at) VALUES ($1,$2,'failed','MODEL_ERROR',true,'2026-10-23',false,$3)`, generationAPIUser, q, generationAPINow)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	second := generationAPIAsk(t, env, generationAPIUser, generationAPIKey(), "Второй финансовый вопрос", commonv1.TransferMode_TRANSFER_MODE_WITH)
	generationAPITerminal(t, env, generationAPIUser, second.AnswerId)
	m.mu.Lock()
	defer m.mu.Unlock()
	input := string(m.requests[1].body["input"])
	if !strings.Contains(input, "Первый финансовый вопрос") || !strings.Contains(input, "Финансовый ответ") || !strings.Contains(input, "Второй финансовый вопрос") || strings.Contains(input, "FAILED_QUESTION") {
		t.Fatalf("invalid history window: %s", input)
	}
}
func TestGenerationAPIModelCannotChooseAnotherUser(t *testing.T) {
	p := &generationAPIPeers{grant: true}
	m := generationAPIModel(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		args := fmt.Sprintf(`{"all_time":true,"group_by":[],"user_id":%q}`, generationAPIOther)
		_, _ = io.WriteString(w, generationAPIFunction("aggregate_operations", args))
	})
	env := generationAPIEnv(t, p, m)
	accepted := generationAPIAsk(t, env, generationAPIUser, generationAPIKey(), "Расходы", commonv1.TransferMode_TRANSFER_MODE_WITH)
	done, failed := generationAPITerminal(t, env, generationAPIUser, accepted.AnswerId)
	if done != nil || failed == nil || failed.Code != assistantv1.AnswerErrorCode_ANSWER_ERROR_CODE_MODEL_ERROR || failed.RemainingLimit != 5 || p.count("aggregate") != 0 || m.count.Load() != 1 {
		t.Fatalf("%v %v", done, failed)
	}
}
func TestGenerationAPIUnconfiguredModelFailsWithoutCreatingQuestion(t *testing.T) {
	env := apitest.Start(t)
	c := assistantv1.NewAssistantServiceClient(env.Conn)
	_, err := c.AskQuestion(generationAPIContext(t, generationAPIUser, generationAPIKey()), &assistantv1.AskQuestionRequest{Text: "Финансы"})
	generationAPIAssertError(t, err, codes.Unavailable, "")
	_, err = c.RetryAnswer(generationAPIContext(t, generationAPIUser, generationAPIKey()), &assistantv1.RetryAnswerRequest{AnswerId: generationAPIKey()})
	generationAPIAssertError(t, err, codes.Unavailable, "")
	limit, err := c.GetLimit(generationAPIContext(t, generationAPIUser, ""), &assistantv1.GetLimitRequest{})
	if err != nil || limit.Remaining != 5 || generationAPICount(t, env, "question") != 0 || generationAPICount(t, env, "key") != 0 {
		t.Fatalf("%v %v", limit, err)
	}
}

func TestGenerationAPIUnaryCancellationDuringEffectCommitsAndReplays(t *testing.T) {
	p := &generationAPIPeers{grant: true}
	m := generationAPIModel(t, nil)
	env := generationAPIEnv(t, p, m)
	c := assistantv1.NewAssistantServiceClient(env.Conn)
	// Establish an existing root so the effect waits in its user-row SELECT,
	// after the real shared engine has fenced the committed key reservation.
	if _, err := env.App.DB.Writer(t.Context()).Exec(t.Context(), `INSERT INTO user_state(user_id,limit_day,limit_used) VALUES ($1,'2026-10-23',0)`, generationAPIUser); err != nil {
		t.Fatal(err)
	}
	blocker, err := pgx.Connect(t.Context(), env.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Close(context.Background()) }()
	hold, err := blocker.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hold.Rollback(context.Background()) }()
	var blockerPID, used int
	if err := hold.QueryRow(t.Context(), `SELECT pg_backend_pid(),limit_used FROM user_state WHERE user_id=$1 FOR UPDATE`, generationAPIUser).Scan(&blockerPID, &used); err != nil || used != 0 {
		t.Fatalf("lock existing counter: used%d %v", used, err)
	}
	key := generationAPIKey()
	request := &assistantv1.AskQuestionRequest{Text: "Вопрос принят до отмены unary"}
	unary, cancel := context.WithCancel(generationAPIContext(t, generationAPIUser, key))
	defer cancel()
	outcome := make(chan error, 1)
	go func() { _, err := c.AskQuestion(unary, request); outcome <- err }()
	// A lock graph is the synchronization condition, rather than a sleep or
	// successful Ask response: this exact query runs inside DoReserved's effect.
	waiting, stopWaiting := context.WithTimeout(t.Context(), 5*time.Second)
	defer stopWaiting()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	var servicePID int
	for servicePID == 0 {
		select {
		case <-waiting.Done():
			t.Fatal("Ask did not enter the fenced effect before cancellation")
		case err := <-outcome:
			t.Fatalf("Ask returned before cancellation: %v", err)
		case <-tick.C:
		}
		err := env.App.DB.Writer(waiting).QueryRow(waiting, `SELECT pid FROM pg_stat_activity WHERE datname=current_database() AND $1::integer=ANY(pg_blocking_pids(pid)) AND query LIKE 'SELECT limit_day,limit_used FROM user_state%'`, blockerPID).Scan(&servicePID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			t.Fatal(err)
		}
	}
	// The request is still waiting for its counter lock: no question, charge or
	// model POST has happened. Its separately committed reservation is unfinished.
	var done bool
	if err := env.App.DB.Writer(t.Context()).QueryRow(t.Context(), `SELECT done FROM idempotency_keys WHERE scope=$1 AND key=$2`, generationAPIUser, key).Scan(&done); err != nil || done {
		t.Fatalf("expected fenced pending key: done%v %v", done, err)
	}
	if m.count.Load() != 0 || generationAPICount(t, env, "question") != 0 || generationAPICount(t, env, "answer") != 0 {
		t.Fatal("effect completed while row lock was held")
	}
	cancel()
	select {
	case err := <-outcome:
		generationAPIAssertError(t, err, codes.Canceled, "")
	case <-time.After(5 * time.Second):
		t.Fatal("canceled unary did not return")
	}
	// Release only after the client has observed cancellation of the still
	// executing unary. The server must finish using its detached effect context.
	if err := hold.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	generationAPIWaitArrival(t, m) // Start runs only after the acceptance commit.
	p.consent(false, false)
	consentCalls := p.count("consent")
	replay, err := c.AskQuestion(generationAPIContext(t, generationAPIUser, key), request)
	if err != nil {
		t.Fatalf("committed canceled request did not replay: %v", err)
	}
	if consentCalls != 1 || p.count("consent") != consentCalls || replay.RemainingLimit != 4 {
		t.Fatalf("replay ran consent or charged again: calls%d replay%v", p.count("consent"), replay)
	}
	finished, failed := generationAPITerminal(t, env, generationAPIUser, replay.AnswerId)
	if failed != nil || finished == nil || !finished.Charged || finished.RemainingLimit != 4 {
		t.Fatalf("terminal %v %v", finished, failed)
	}
	var questionID, answerID string
	if err := env.App.DB.Writer(t.Context()).QueryRow(t.Context(), `SELECT q.id::text,a.id::text,s.limit_used,k.done FROM question q JOIN answer a ON a.user_id=q.user_id AND a.question_id=q.id JOIN user_state s ON s.user_id=q.user_id JOIN idempotency_keys k ON k.scope=q.user_id WHERE q.user_id=$1 AND k.key=$2`, generationAPIUser, key).Scan(&questionID, &answerID, &used, &done); err != nil {
		t.Fatal(err)
	}
	if questionID != replay.GetQuestion().GetId() || answerID != replay.AnswerId || used != 1 || !done || m.count.Load() != 1 || generationAPICount(t, env, "question") != 1 || generationAPICount(t, env, "answer") != 1 || generationAPICount(t, env, "key") != 1 {
		t.Fatalf("canceled acceptance was not exactly once: used%d done%v requests%d replay%v", used, done, m.count.Load(), replay)
	}
}
