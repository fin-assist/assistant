package dialogue

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"google.golang.org/genproto/googleapis/rpc/errdetails"

	assistantv1 "github.com/fin-assist/contracts/gen/go/finassist/assistant/v1"
	"github.com/fin-assist/contracts/gen/go/finerr"

	"github.com/fin-assist/go-grpc/reqctx"

	domain "github.com/fin-assist/assistant/internal/dialogue"
)

// Reads is the history and answer observation behavior the transport consumes.
type Reads interface {
	List(context.Context, string, string, int) (domain.Page, error)
	Watch(context.Context, string, string, func(domain.Event) error) error
}

// Handler validates read requests and translates durable updates to the generated API.
type Handler struct {
	service      Reads
	offTopicText string
}

// Options supplies compatibility text retained until all consumers and rollback images are upgraded.
type Options struct{ OffTopicText string }

// New wires the consumer-side interface.
func New(service Reads, options ...Options) *Handler {
	text := defaultOffTopicText
	if len(options) > 0 && options[0].OffTopicText != "" {
		text = options[0].OffTopicText
	}
	return &Handler{service: service, offTopicText: text}
}

// ListMessages validates pagination and preserves oldest-first response order.
func (h *Handler) ListMessages(ctx context.Context, req *assistantv1.ListMessagesRequest) (*assistantv1.ListMessagesResponse, error) {
	user, err := subject(ctx)
	if err != nil {
		return nil, err
	}
	limit := int(req.GetLimit())
	if limit == 0 {
		limit = 20
	}
	if limit < 1 || limit > 50 {
		return nil, invalid("limit", "OUT_OF_RANGE")
	}
	before := ""
	if req.BeforeId != nil {
		var err error
		before, err = identifier(req.GetBeforeId(), "before_id")
		if err != nil {
			return nil, err
		}
	}
	page, err := h.service.List(ctx, user, before, limit)
	if err != nil {
		return nil, readError(err)
	}
	result := &assistantv1.ListMessagesResponse{HasOlder: page.HasOlder, TimeZone: page.TimeZone, Messages: []*assistantv1.Message{}}
	for _, saved := range page.Messages {
		m, err := message(saved, h.offTopicText)
		if err != nil {
			return nil, err
		}
		result.Messages = append(result.Messages, m)
	}
	return result, nil
}

// WatchAnswer emits a first snapshot and forwards saved updates. A transport failure
// terminates the RPC, without fabricating a terminal error event for the answer.
func (h *Handler) WatchAnswer(req *assistantv1.WatchAnswerRequest, stream assistantv1.AssistantService_WatchAnswerServer) error {
	user, err := subject(stream.Context())
	if err != nil {
		return err
	}
	id, err := identifier(req.GetAnswerId(), "answer_id")
	if err != nil {
		return err
	}
	err = h.service.Watch(stream.Context(), user, id, func(event domain.Event) error {
		message := &assistantv1.WatchAnswerResponse{TimeZone: event.Answer.TimeZone}
		switch event.Kind {
		case "snapshot":
			snapshot, err := AnswerSnapshot(*event.Answer)
			if err != nil {
				return err
			}
			message.Event = &assistantv1.WatchAnswerResponse_Snapshot{Snapshot: snapshot}
		case "chunk":
			message.Event = &assistantv1.WatchAnswerResponse_Chunk{Chunk: &assistantv1.AnswerChunk{Index: int32(event.Index), Text: event.Text}}
		case "block":
			blocks, err := Blocks([]domain.Block{*event.Block})
			if err != nil {
				return err
			}
			if text := blocks[0].GetText(); text != nil {
				message.Event = &assistantv1.WatchAnswerResponse_Chunk{Chunk: &assistantv1.AnswerChunk{Index: int32(event.Index), Text: text.Text}}
			} else {
				message.Event = &assistantv1.WatchAnswerResponse_Block{Block: &assistantv1.AnswerBlock{Index: int32(event.Index), Block: blocks[0]}}
			}
		case "source":
			source, err := Source(event.Source)
			if err != nil {
				return err
			}
			message.Event = &assistantv1.WatchAnswerResponse_Source{Source: source}
		case "chip":
			chips, err := Chips([]json.RawMessage{event.Chip})
			if err != nil {
				return err
			}
			message.Event = &assistantv1.WatchAnswerResponse_Chip{Chip: chips[0]}
		case "done":
			message.Event = &assistantv1.WatchAnswerResponse_Done{Done: &assistantv1.AnswerDone{Charged: event.Answer.Charged, RemainingLimit: int32(event.Remaining)}}
		case "error":
			failure, err := errorWithText(*event.Answer, event.Remaining, h.offTopicText)
			if err != nil {
				return err
			}
			message.Event = &assistantv1.WatchAnswerResponse_Error{Error: failure}
		default:
			return errors.New("invalid saved answer event")
		}
		return stream.Send(message)
	})
	return readError(err)
}

func subject(ctx context.Context) (string, error) {
	id, ok := reqctx.UserID(ctx)
	if !ok {
		return "", finerr.InvalidToken()
	}
	u, err := uuid.Parse(id)
	if err != nil {
		return "", finerr.InvalidToken()
	}
	return u.String(), nil
}
func identifier(id, field string) (string, error) {
	if id == "" {
		return "", invalid(field, "REQUIRED")
	}
	u, err := uuid.Parse(id)
	if err != nil {
		return "", invalid(field, "INVALID_FORMAT")
	}
	return u.String(), nil
}
func invalid(field, reason string) error {
	return finerr.ValidationError(&errdetails.BadRequest_FieldViolation{Field: field, Reason: reason, Description: "Invalid field"})
}
func readError(err error) error {
	if errors.Is(err, domain.ErrNotFound) {
		return finerr.NotFound()
	}
	return err
}
