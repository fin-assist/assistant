package account

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	assistantv1 "github.com/fin-assist/contracts/gen/go/finassist/assistant/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	"github.com/fin-assist/contracts/gen/go/finerr"
	idem "github.com/fin-assist/go-idempotency"
	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/fin-assist/assistant/internal/asking"
	"github.com/fin-assist/assistant/internal/domain"
	"github.com/fin-assist/assistant/internal/reqctx"
)

const answerID = "01a10852-b341-7a10-8f3e-6c2d9b4a1e07"
const keyID = "00000000-0000-4000-8000-000000000002"

type noKeyTX struct{}

func (noKeyTX) Tx(context.Context) (pgx.Tx, bool) { return nil, false }
func keyEngine(t *testing.T) *idem.Engine {
	t.Helper()
	engine, err := idem.New(noKeyTX{}, idem.Options{Methods: map[string]idem.Policy{assistantv1.AssistantService_AskQuestion_FullMethodName: {}, assistantv1.AssistantService_RetryAnswer_FullMethodName: {}}, Clock: time.Now, Registerer: prometheus.NewRegistry()})
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

type fakeAsking struct {
	reply                    asking.Reply
	err                      error
	calls                    int
	user, text, mode, answer string
	key                      idem.Request
}

func (f *fakeAsking) Ask(_ context.Context, user string, key idem.Request, text, mode string) (asking.Reply, error) {
	f.calls++
	f.user = user
	f.key = key
	f.text = text
	f.mode = mode
	return f.reply, f.err
}
func (f *fakeAsking) Retry(_ context.Context, user string, key idem.Request, answer string) (asking.Reply, error) {
	f.calls++
	f.user = user
	f.key = key
	f.answer = answer
	return f.reply, f.err
}
func askHandler(t *testing.T) (*Handler, *fakeAsking) {
	t.Helper()
	f := &fakeAsking{reply: asking.Reply{Task: domain.GenerationTask{UserID: id, QuestionID: "question", AnswerID: answerID, Text: "saved text", QuestionCreatedAt: time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)}, Remaining: 4}}
	return &Handler{asking: f, keyMetadata: keyEngine(t)}, f
}
func caller(t *testing.T) context.Context {
	t.Helper()
	return reqctx.WithUserID(metadata.NewIncomingContext(t.Context(), metadata.Pairs("idempotency-key", keyID)), id)
}
func reason(t *testing.T, err error, want string) {
	t.Helper()
	r, ok := finerr.ToREST(err)
	if !ok || r.Code != want {
		t.Fatalf("got %v want %s", err, want)
	}
}
func TestUnitAskQuestionMappingAndHash(t *testing.T) {
	for _, tt := range []struct {
		mode commonv1.TransferMode
		want string
	}{{commonv1.TransferMode_TRANSFER_MODE_UNSPECIFIED, "with"}, {commonv1.TransferMode_TRANSFER_MODE_WITH, "with"}, {commonv1.TransferMode_TRANSFER_MODE_WITHOUT, "without"}} {
		t.Run(tt.want+tt.mode.String(), func(t *testing.T) {
			h, f := askHandler(t)
			req := &assistantv1.AskQuestionRequest{Text: strings.Repeat("界", 2000), TransferMode: tt.mode}
			out, err := h.AskQuestion(caller(t), req)
			hash, _ := idem.HashProto(req)
			if err != nil || out.Question.GetText() != "saved text" || out.Question.Id != "question" || out.Question.Role != assistantv1.Role_ROLE_USER || out.Question.Currency != "RUB" || !out.Question.CreatedAt.AsTime().Equal(f.reply.Task.QuestionCreatedAt) || out.AnswerId != answerID || out.RemainingLimit != 4 || f.user != id || f.text != req.Text || f.mode != tt.want || f.key.Hash != hash || f.key.Scope != id || f.key.Key != keyID || f.key.Endpoint != assistantv1.AssistantService_AskQuestion_FullMethodName {
				t.Fatalf("%+v %v %+v", out, err, f)
			}
		})
	}
}
func TestUnitAskingValidationBeforeUseCase(t *testing.T) {
	for _, tt := range []struct {
		name string
		req  *assistantv1.AskQuestionRequest
	}{{"nil", nil}, {"empty", &assistantv1.AskQuestionRequest{}}, {"long", &assistantv1.AskQuestionRequest{Text: strings.Repeat("я", 2001)}}, {"utf8", &assistantv1.AskQuestionRequest{Text: string([]byte{0xff})}}, {"unknown mode", &assistantv1.AskQuestionRequest{Text: "q", TransferMode: commonv1.TransferMode(99)}}} {
		t.Run(tt.name, func(t *testing.T) {
			h, f := askHandler(t)
			_, err := h.AskQuestion(caller(t), tt.req)
			reason(t, err, "VALIDATION_ERROR")
			if f.calls != 0 {
				t.Fatal("usecase reached")
			}
		})
	}
	for _, value := range []string{"", "bad", strings.ToUpper(answerID), "{01a10852-b341-7a10-8f3e-6c2d9b4a1e07}"} {
		t.Run(value, func(t *testing.T) {
			h, f := askHandler(t)
			_, err := h.RetryAnswer(caller(t), &assistantv1.RetryAnswerRequest{AnswerId: value})
			reason(t, err, "VALIDATION_ERROR")
			if f.calls != 0 {
				t.Fatal("usecase reached")
			}
		})
	}
	h, f := askHandler(t)
	if _, err := h.RetryAnswer(caller(t), nil); err == nil || f.calls != 0 {
		t.Fatal(err)
	}
}
func TestUnitVerifiedSubjectAndKeyMetadata(t *testing.T) {
	for _, user := range []string{"", "not uuid"} {
		h, f := askHandler(t)
		_, err := h.AskQuestion(reqctx.WithUserID(t.Context(), user), &assistantv1.AskQuestionRequest{Text: "q"})
		reason(t, err, "INVALID_TOKEN")
		if f.calls != 0 {
			t.Fatal("identity bypass")
		}
	}
	for _, md := range []metadata.MD{nil, metadata.Pairs("idempotency-key", "invalid"), metadata.Pairs("idempotency-key", keyID, "idempotency-key", keyID)} {
		h, f := askHandler(t)
		_, err := h.AskQuestion(reqctx.WithUserID(metadata.NewIncomingContext(t.Context(), md), id), &assistantv1.AskQuestionRequest{Text: "q"})
		reason(t, err, "VALIDATION_ERROR")
		if f.calls != 0 {
			t.Fatal("invalid key reached effect")
		}
	}
}
func TestUnitRetryMappingAndDomainErrors(t *testing.T) {
	h, f := askHandler(t)
	req := &assistantv1.RetryAnswerRequest{AnswerId: answerID}
	got, err := h.RetryAnswer(caller(t), req)
	hash, _ := idem.HashProto(req)
	if err != nil || got.AnswerId != answerID || got.RemainingLimit != 4 || f.answer != answerID || f.text != "" || f.mode != "" || f.key.Hash != hash || f.key.Endpoint != assistantv1.AssistantService_RetryAnswer_FullMethodName {
		t.Fatalf("%+v %v %+v", got, err, f)
	}
	for _, tt := range []struct {
		err    error
		reason string
		code   codes.Code
	}{{asking.ErrNotFound, "NOT_FOUND", codes.NotFound}, {asking.ErrRetryDenied, "RETRY_NOT_ALLOWED", codes.FailedPrecondition}, {asking.ErrDeleted, "TOKEN_REVOKED", codes.Unauthenticated}, {asking.ErrConsent, "CONSENT_REQUIRED", codes.PermissionDenied}, {&asking.LimitError{ResetsAt: time.Now().Add(time.Hour)}, "LIMIT_EXCEEDED", codes.PermissionDenied}, {asking.ErrUnavailable, "", codes.Unavailable}} {
		t.Run(tt.err.Error(), func(t *testing.T) {
			h, f := askHandler(t)
			f.err = tt.err
			_, err := h.RetryAnswer(caller(t), req)
			if tt.reason != "" {
				reason(t, err, tt.reason)
			} else if status.Code(err) != tt.code {
				t.Fatal(err)
			}
			_, err = h.AskQuestion(caller(t), &assistantv1.AskQuestionRequest{Text: "q"})
			if tt.reason != "" {
				reason(t, err, tt.reason)
			} else if status.Code(err) != tt.code {
				t.Fatal(err)
			}
		})
	}
	unknown := errors.New("private internal failure")
	if !errors.Is(askingError(unknown), unknown) {
		t.Fatal("internal error must reach server normalizer")
	}
	h, f = askHandler(t)
	f.reply.Task.QuestionCreatedAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	_, err = h.AskQuestion(caller(t), &assistantv1.AskQuestionRequest{Text: "q"})
	reason(t, err, "INTERNAL_ERROR")
}
func TestUnitOptionalAskingRegistrationKeepsUnimplemented(t *testing.T) {
	for _, opts := range [][]AskingOptions{nil, {{}}, {{UseCases: &fakeAsking{}}}, {{Metadata: keyEngine(t)}}} {
		server := grpc.NewServer()
		Register(server, &fakeUseCases{}, nil, opts...)
		server.Stop()
		h := &Handler{}
		if len(opts) > 0 {
			h.asking = opts[0].UseCases
			h.keyMetadata = opts[0].Metadata
		}
		if _, err := h.AskQuestion(t.Context(), nil); status.Code(err) != codes.Unimplemented {
			t.Fatal(err)
		}
		if _, err := h.RetryAnswer(t.Context(), nil); status.Code(err) != codes.Unimplemented {
			t.Fatal(err)
		}
	}
}

// A metadata adapter that fails to install the library context must fail closed.
type missingKeyContext struct{}

func (missingKeyContext) UnaryInterceptor(func(context.Context) (string, error)) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
		return next(ctx, req)
	}
}
func TestUnitMissingLibraryKeyContextFailsClosed(t *testing.T) {
	h, f := askHandler(t)
	h.keyMetadata = missingKeyContext{}
	_, err := h.AskQuestion(caller(t), &assistantv1.AskQuestionRequest{Text: "q"})
	reason(t, err, "INTERNAL_ERROR")
	if f.calls != 0 {
		t.Fatal("missing key context reached usecase")
	}
}

func TestUnitOffTopicRefusalCarriesCanonicalRetryMetadataForAskAndRetry(t *testing.T) {
	retryAt := time.Now().Add(time.Hour).In(time.FixedZone("MSK", 3*60*60)).Truncate(time.Second)
	for _, retry := range []bool{false, true} {
		t.Run(map[bool]string{false: "ask", true: "retry"}[retry], func(t *testing.T) {
			h, f := askHandler(t)
			f.err = fmt.Errorf("private repository context: %w", &asking.RateLimitError{RetryAt: retryAt})
			var err error
			if retry {
				var out *assistantv1.RetryAnswerResponse
				out, err = h.RetryAnswer(caller(t), &assistantv1.RetryAnswerRequest{AnswerId: answerID})
				if out != nil {
					t.Fatal("refusal returned a successful acceptance")
				}
			} else {
				var out *assistantv1.AskQuestionResponse
				out, err = h.AskQuestion(caller(t), &assistantv1.AskQuestionRequest{Text: "question"})
				if out != nil {
					t.Fatal("refusal returned a successful acceptance")
				}
			}
			st := status.Convert(err)
			if st.Code() != codes.ResourceExhausted || st.Message() != "RATE_LIMITED" || f.calls != 1 {
				t.Fatalf("noncanonical refusal: %v", err)
			}
			var info *errdetails.ErrorInfo
			var retryInfo *errdetails.RetryInfo
			for _, detail := range st.Details() {
				switch value := detail.(type) {
				case *errdetails.ErrorInfo:
					info = value
				case *errdetails.RetryInfo:
					retryInfo = value
				}
			}
			if info == nil || info.Domain != finerr.Domain || info.Reason != "RATE_LIMITED" || len(info.Metadata) != 1 || info.Metadata["retry_at"] != retryAt.Format(time.RFC3339) {
				t.Fatalf("retry_at or sanitized metadata lost: %+v", info)
			}
			if retryInfo == nil || retryInfo.RetryDelay.AsDuration() <= 0 || retryInfo.RetryDelay.AsDuration() > time.Hour+time.Second {
				t.Fatalf("retry delay missing or unbounded: %+v", retryInfo)
			}
			rest, ok := finerr.ToREST(err)
			if !ok || rest.Spec.HTTPStatus != 429 || rest.RetryAfter <= 0 || rest.RetryAfter%time.Second != 0 {
				t.Fatalf("REST Retry-After projection lost: %+v", rest)
			}
		})
	}
}
