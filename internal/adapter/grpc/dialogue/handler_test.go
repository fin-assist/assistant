package dialogue

import (
	"context"
	"errors"
	"testing"
	"time"

	assistantv1 "github.com/fin-assist/contracts/gen/go/finassist/assistant/v1"
	"github.com/fin-assist/contracts/gen/go/finerr"
	"google.golang.org/grpc"

	domain "github.com/fin-assist/assistant/internal/dialogue"
	"github.com/fin-assist/assistant/internal/reqctx"
)

const user = "00000000-0000-4000-8000-000000000001"

type reads struct {
	page                domain.Page
	events              []domain.Event
	err                 error
	limit               int
	before, id, subject string
	calls               int
}

func (r *reads) List(_ context.Context, u, b string, n int) (domain.Page, error) {
	r.calls++
	r.limit = n
	r.subject = u
	r.before = b
	return r.page, r.err
}
func (r *reads) Watch(_ context.Context, u, id string, send func(domain.Event) error) error {
	r.calls++
	r.id = id
	r.subject = u
	for _, e := range r.events {
		if err := send(e); err != nil {
			return err
		}
	}
	return r.err
}

type stream struct {
	grpc.ServerStream
	ctx      context.Context
	messages []*assistantv1.WatchAnswerResponse
	err      error
}

func (s *stream) Context() context.Context { return s.ctx }
func (s *stream) Send(m *assistantv1.WatchAnswerResponse) error {
	s.messages = append(s.messages, m)
	return s.err
}
func TestHistoryHandler(t *testing.T) {
	r := &reads{page: domain.Page{TimeZone: "UTC"}}
	h := New(r)
	ctx := reqctx.WithUserID(t.Context(), user)
	result, err := h.ListMessages(ctx, &assistantv1.ListMessagesRequest{})
	if err != nil || r.limit != 20 || result.TimeZone != "UTC" || result.Messages == nil {
		t.Fatalf("%+v %v", result, err)
	}
	cursor := user
	_, err = h.ListMessages(ctx, &assistantv1.ListMessagesRequest{Limit: 50, BeforeId: &cursor})
	if err != nil || r.limit != 50 || r.before != cursor {
		t.Fatal(err)
	}
	for _, u := range []string{"", "invalid"} {
		if _, err := h.ListMessages(reqctx.WithUserID(t.Context(), u), &assistantv1.ListMessagesRequest{}); err == nil {
			t.Fatal("invalid subject accepted")
		}
	}
	for _, n := range []int32{-1, 51} {
		if _, err := h.ListMessages(ctx, &assistantv1.ListMessagesRequest{Limit: n}); err == nil {
			t.Fatal("invalid limit accepted")
		}
	}
	for _, b := range []string{"", "bad"} {
		if _, err := h.ListMessages(ctx, &assistantv1.ListMessagesRequest{BeforeId: &b}); err == nil {
			t.Fatal("invalid cursor accepted")
		}
	}
	r.err = domain.ErrNotFound
	if _, err := h.ListMessages(ctx, &assistantv1.ListMessagesRequest{}); !errors.Is(err, finerr.NotFound()) {
		if err == nil {
			t.Fatal(err)
		}
	}
	r.err = nil
	r.page.Messages = []domain.Message{{ID: "q", CreatedAt: time.Now(), Question: "hello"}}
	if result, err := h.ListMessages(ctx, &assistantv1.ListMessagesRequest{}); err != nil || result.Messages[0].GetText() != "hello" {
		t.Fatalf("%+v %v", result, err)
	}
	r.page.Messages[0].CreatedAt = time.Time{}
	if _, err := h.ListMessages(ctx, &assistantv1.ListMessagesRequest{}); err == nil {
		t.Fatal("invalid saved date accepted")
	}
}
func TestWatchHandlerEvents(t *testing.T) {
	a := &domain.Answer{Status: "complete", Currency: "RUB", TimeZone: "UTC", ErrorCode: "MODEL_ERROR"}
	source := &domain.Source{CalculatedAt: time.Now(), TransferMode: "with"}
	r := &reads{events: []domain.Event{
		{Kind: "snapshot", Answer: a}, {Kind: "chunk", Answer: a, Index: 0, Text: "hi"},
		{Kind: "block", Answer: a, Index: 1, Block: &domain.Block{Type: "text", Version: 1, Text: "fallback"}},
		{Kind: "source", Answer: a, Source: source},
		{Kind: "chip", Answer: a, Chip: []byte(`{"label":"Данные","screen":"analytics","params":{"period":"month","date":"2026-10","transfer_mode":"with"}}`)},
		{Kind: "done", Answer: a, Remaining: 3}, {Kind: "error", Answer: &domain.Answer{Status: "failed", ErrorCode: "MODEL_ERROR", TimeZone: "UTC"}, Remaining: 5},
	}}
	h := New(r)
	s := &stream{ctx: reqctx.WithUserID(t.Context(), user)}
	if err := h.WatchAnswer(&assistantv1.WatchAnswerRequest{AnswerId: user}, s); err != nil || len(s.messages) != 7 {
		t.Fatalf("%v %d", err, len(s.messages))
	}
	if s.messages[2].GetChunk().Text != "fallback" || s.messages[2].GetChunk().Index != 1 || s.messages[1].GetChunk().Text != "hi" || s.messages[5].GetDone().RemainingLimit != 3 || s.messages[6].GetError().RemainingLimit != 5 {
		t.Fatal(s.messages)
	}
	for _, e := range []domain.Event{{Kind: "unknown", Answer: a}, {Kind: "snapshot", Answer: &domain.Answer{Status: "invalid"}}, {Kind: "block", Answer: a, Block: &domain.Block{}}, {Kind: "source", Answer: a, Source: &domain.Source{}}, {Kind: "chip", Answer: a, Chip: []byte(`bad`)}, {Kind: "error", Answer: &domain.Answer{ErrorCode: "UNKNOWN"}}} {
		r.events = []domain.Event{e}
		if err := h.WatchAnswer(&assistantv1.WatchAnswerRequest{AnswerId: user}, s); err == nil {
			t.Fatalf("accepted %+v", e)
		}
	}
	r.events = nil
	r.err = domain.ErrNotFound
	if err := h.WatchAnswer(&assistantv1.WatchAnswerRequest{AnswerId: user}, s); err == nil {
		t.Fatal(err)
	}
	for _, id := range []string{"", "bad"} {
		if err := h.WatchAnswer(&assistantv1.WatchAnswerRequest{AnswerId: id}, s); err == nil {
			t.Fatal("invalid id accepted")
		}
	}
	s.ctx = t.Context()
	if err := h.WatchAnswer(&assistantv1.WatchAnswerRequest{AnswerId: user}, s); err == nil {
		t.Fatal("missing identity")
	}
}
