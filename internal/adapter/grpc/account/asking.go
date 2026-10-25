package account

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	assistantv1 "github.com/fin-assist/contracts/gen/go/finassist/assistant/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	"github.com/fin-assist/contracts/gen/go/finerr"
	idem "github.com/fin-assist/go-idempotency"

	"github.com/fin-assist/go-grpc/reqctx"

	keyadapter "github.com/fin-assist/assistant/internal/adapter/idempotency"
	"github.com/fin-assist/assistant/internal/asking"
)

// AskingUseCases is the acceptance behavior consumed by the generated RPC handlers.
type AskingUseCases interface {
	Ask(context.Context, string, idem.Request, string, string) (asking.Reply, error)
	Retry(context.Context, string, idem.Request, string) (asking.Reply, error)
}

// KeyMetadata validates the shared library's key format after verified authentication.
type KeyMetadata interface {
	UnaryInterceptor(func(context.Context) (string, error)) grpc.UnaryServerInterceptor
}

// AskingOptions enables both acceptance methods; nil dependencies keep them unimplemented.
type AskingOptions struct {
	UseCases AskingUseCases
	Metadata KeyMetadata
}

// AskQuestion accepts the verified user's question with a deterministic protobuf hash.
func (h *Handler) AskQuestion(ctx context.Context, req *assistantv1.AskQuestionRequest) (*assistantv1.AskQuestionResponse, error) {
	if h.asking == nil || h.keyMetadata == nil {
		return h.UnimplementedAssistantServiceServer.AskQuestion(ctx, req)
	}
	out, err := h.withKey(ctx, req, assistantv1.AssistantService_AskQuestion_FullMethodName, func(ctx context.Context, user string, key idem.Request) (any, error) {
		text := req.GetText()
		if !utf8.ValidString(text) || utf8.RuneCountInString(text) < 1 || utf8.RuneCountInString(text) > 2000 {
			return nil, violation("text", "OUT_OF_RANGE")
		}
		// api.md 7.1: an empty text is VALIDATION_ERROR. A blank text is empty
		// for the model, which would fail it as MODEL_ERROR on every retry; NUL
		// cannot be stored in PostgreSQL text and would surface as INTERNAL.
		if strings.TrimSpace(text) == "" {
			return nil, violation("text", "REQUIRED")
		}
		if strings.ContainsRune(text, 0) {
			return nil, violation("text", "INVALID_FORMAT")
		}
		mode := "with"
		switch req.GetTransferMode() {
		case commonv1.TransferMode_TRANSFER_MODE_UNSPECIFIED, commonv1.TransferMode_TRANSFER_MODE_WITH:
		case commonv1.TransferMode_TRANSFER_MODE_WITHOUT:
			mode = "without"
		default:
			return nil, violation("transfer_mode", "INVALID_FORMAT")
		}
		reply, err := h.asking.Ask(ctx, user, key, text, mode)
		if err != nil {
			return nil, askingError(err)
		}
		at := timestamppb.New(reply.Task.QuestionCreatedAt)
		if err := at.CheckValid(); err != nil {
			return nil, finerr.InternalError()
		}
		return &assistantv1.AskQuestionResponse{Question: &assistantv1.Message{Id: reply.Task.QuestionID, Role: assistantv1.Role_ROLE_USER, Text: proto.String(reply.Task.Text), CreatedAt: at, Currency: "RUB"}, AnswerId: reply.Task.AnswerID, RemainingLimit: int32(reply.Remaining)}, nil
	})
	if err != nil {
		return nil, err
	}
	return out.(*assistantv1.AskQuestionResponse), nil
}

// RetryAnswer replaces a saved failed answer; text and transfer mode stay server-owned.
func (h *Handler) RetryAnswer(ctx context.Context, req *assistantv1.RetryAnswerRequest) (*assistantv1.RetryAnswerResponse, error) {
	if h.asking == nil || h.keyMetadata == nil {
		return h.UnimplementedAssistantServiceServer.RetryAnswer(ctx, req)
	}
	out, err := h.withKey(ctx, req, assistantv1.AssistantService_RetryAnswer_FullMethodName, func(ctx context.Context, user string, key idem.Request) (any, error) {
		answer, err := uuid.Parse(req.GetAnswerId())
		if err != nil || answer.String() != req.GetAnswerId() {
			return nil, violation("answer_id", "INVALID_FORMAT")
		}
		reply, err := h.asking.Retry(ctx, user, key, answer.String())
		if err != nil {
			return nil, askingError(err)
		}
		return &assistantv1.RetryAnswerResponse{AnswerId: reply.Task.AnswerID, RemainingLimit: int32(reply.Remaining)}, nil
	})
	if err != nil {
		return nil, err
	}
	return out.(*assistantv1.RetryAnswerResponse), nil
}

func (h *Handler) withKey(ctx context.Context, req proto.Message, method string, next func(context.Context, string, idem.Request) (any, error)) (any, error) {
	scope := func(ctx context.Context) (string, error) {
		user, ok := reqctx.UserID(ctx)
		parsed, err := uuid.Parse(user)
		if !ok || err != nil {
			return "", finerr.InvalidToken()
		}
		return parsed.String(), nil
	}
	// The server's auth/tombstone interceptors have already run. Invoke the shared
	// metadata adapter here so unauthenticated callers cannot reach key handling.
	return h.keyMetadata.UnaryInterceptor(scope)(ctx, req, &grpc.UnaryServerInfo{FullMethod: method}, func(ctx context.Context, _ any) (any, error) {
		key, ok := idem.FromContext(ctx)
		if !ok {
			return nil, finerr.InternalError()
		}
		hash, err := idem.HashProto(req)
		if err != nil {
			return nil, violation("request", "INVALID_FORMAT")
		}
		key.Hash = hash
		return next(ctx, key.Scope, key)
	})
}

func askingError(err error) error {
	if errors.Is(err, asking.ErrUnavailable) {
		return unavailable()
	}
	return keyadapter.ContractError(err)
}
