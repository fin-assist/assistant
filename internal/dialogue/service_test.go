package dialogue

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"
)

type saved struct {
	page    Page
	answers []Answer
	err     error
	failAt  int
	calls   int
}

func (r *saved) List(context.Context, string, string, int) (Page, error) { return r.page, r.err }
func (r *saved) Snapshot(context.Context, string, string) (Answer, error) {
	r.calls++
	if r.err != nil && (r.failAt == 0 || r.failAt == r.calls) {
		return Answer{}, r.err
	}
	return r.answers[min(r.calls-1, len(r.answers)-1)], nil
}

type versions struct {
	version Version
	calls   int
	err     error
}

func (v *versions) Get(context.Context, string) (Version, error) { v.calls++; return v.version, v.err }

type notices struct {
	ch        chan struct{}
	err       error
	cancelled bool
}

func (n *notices) Subscribe(context.Context, string) (<-chan struct{}, func(), error) {
	return n.ch, func() { n.cancelled = true }, n.err
}

type allowance struct{ err error }

func (a allowance) Remaining(context.Context, string) (int, error) { return 4, a.err }

func TestHistoryOneVersionReadPerPage(t *testing.T) {
	now := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	later := now.Add(time.Second)
	repo := &saved{page: Page{Messages: []Message{{ID: "q", Question: "hello"}, {Answer: &Answer{Source: &Source{CalculatedAt: now}}}, {Answer: &Answer{Source: &Source{CalculatedAt: now}}}}}}
	v := &versions{version: Version{Categories: &now, Uploads: &later}}
	s := New(repo, v, nil, nil)
	page, err := s.List(t.Context(), "u", "", 20)
	if err != nil || v.calls != 1 {
		t.Fatalf("%v calls=%d", err, v.calls)
	}
	for _, m := range page.Messages[1:] {
		if !reflect.DeepEqual(m.Answer.Source.ChangedSince, []string{"uploads"}) {
			t.Fatal(m)
		}
	}
	v.version.Categories = &later
	page, err = s.List(t.Context(), "u", "", 20)
	if err != nil || !reflect.DeepEqual(page.Messages[1].Answer.Source.ChangedSince, []string{"categories", "uploads"}) {
		t.Fatalf("%+v %v", page, err)
	}
	v.err = errors.New("peer")
	if _, err := s.List(t.Context(), "u", "", 20); err == nil {
		t.Fatal("peer error ignored")
	}
	repo.page = Page{}
	v.calls = 0
	if _, err := s.List(t.Context(), "u", "", 20); err != nil || v.calls != 0 {
		t.Fatal(err)
	}
	repo.err = ErrNotFound
	if _, err := s.List(t.Context(), "u", "", 20); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestWatchCumulativeUpdatesAndTerminal(t *testing.T) {
	now := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	initial := Answer{Status: "generating", TimeZone: "UTC", Blocks: []Block{{Type: "text", Version: 1, Text: "Привет"}}}
	complete := initial
	complete.Status = "complete"
	complete.Charged = true
	complete.Blocks = []Block{{Type: "text", Version: 1, Text: "Привет, мир"}, {Type: "rows", Version: 1, Body: json.RawMessage(`{}`)}}
	complete.Chips = []json.RawMessage{json.RawMessage(`{"screen":"analytics"}`)}
	complete.Source = &Source{CalculatedAt: now}
	repo := &saved{answers: []Answer{initial, complete}}
	n := &notices{ch: make(chan struct{}, 1)}
	v := &versions{}
	s := New(repo, v, n, allowance{})
	var got []Event
	err := s.Watch(t.Context(), "u", "a", func(e Event) error {
		got = append(got, e)
		if e.Kind == "snapshot" {
			n.ch <- struct{}{}
		}
		return nil
	})
	kinds := []string{}
	for _, e := range got {
		kinds = append(kinds, e.Kind)
	}
	if err != nil || !reflect.DeepEqual(kinds, []string{"snapshot", "chunk", "block", "source", "chip", "done"}) || got[1].Text != ", мир" || got[1].Index != 0 || got[2].Index != 1 || got[5].Remaining != 4 || !n.cancelled || v.calls != 1 {
		t.Fatalf("%v %v %+v", err, kinds, got)
	}
}

func TestWatchFailures(t *testing.T) {
	boom := errors.New("failed")
	for _, tt := range []struct {
		name                                                       string
		initial, next                                              Answer
		repoErr, errorSend, versionErr, allowanceErr, subscribeErr error
		failAt                                                     int
		closeUpdates                                               bool
		cancel                                                     bool
		want                                                       error
		events                                                     int
	}{
		{name: "complete", initial: Answer{Status: "complete"}, events: 2},
		{name: "failed", initial: Answer{Status: "failed", ErrorCode: "MODEL_ERROR"}, events: 2},
		{name: "invalid state", initial: Answer{Status: "invalid"}, want: boom, events: 1},
		{name: "read error", repoErr: ErrNotFound, want: ErrNotFound},
		{name: "subscribe", subscribeErr: ErrUnavailable, want: ErrUnavailable},
		{name: "send", initial: Answer{Status: "generating"}, errorSend: boom, want: boom, events: 1},
		{name: "version", initial: Answer{Status: "complete", Source: &Source{}}, versionErr: boom, want: boom},
		{name: "allowance", initial: Answer{Status: "complete"}, allowanceErr: boom, want: boom, events: 1},
		{name: "transport closes", initial: Answer{Status: "generating"}, closeUpdates: true, want: ErrUnavailable, events: 1},
		{name: "cancel", initial: Answer{Status: "generating"}, cancel: true, want: context.Canceled, events: 1},
		{name: "read after snapshot", initial: Answer{Status: "generating"}, repoErr: boom, failAt: 2, want: boom, events: 1},
		{name: "generation fails", initial: Answer{Status: "generating", Blocks: []Block{{Type: "text", Version: 1, Text: "partial"}}}, next: Answer{Status: "failed"}, events: 2},
		{name: "invalid next", initial: Answer{Status: "generating"}, next: Answer{Status: "invalid"}, want: boom, events: 1},
		{name: "later peer error", initial: Answer{Status: "generating"}, next: Answer{Status: "complete", Source: &Source{}}, versionErr: boom, want: boom, events: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			n := &notices{ch: make(chan struct{}, 1), err: tt.subscribeErr}
			r := &saved{answers: []Answer{tt.initial, tt.next}, err: tt.repoErr, failAt: tt.failAt}
			s := New(r, &versions{err: tt.versionErr}, n, allowance{err: tt.allowanceErr})
			count := 0
			err := s.Watch(ctx, "u", "a", func(e Event) error {
				count++
				if tt.errorSend != nil {
					return tt.errorSend
				}
				if count == 1 {
					if tt.closeUpdates {
						close(n.ch)
					} else if tt.cancel {
						cancel()
					} else if tt.initial.Status == "generating" {
						n.ch <- struct{}{}
					}
				}
				return nil
			})
			if (err == nil) != (tt.want == nil) || count != tt.events {
				t.Fatalf("error=%v events=%d wantErr=%v events=%d", err, count, tt.want, tt.events)
			}
			if errors.Is(tt.want, ErrNotFound) || errors.Is(tt.want, ErrUnavailable) || errors.Is(tt.want, context.Canceled) || errors.Is(tt.want, boom) && tt.name != "invalid state" && tt.name != "invalid next" {
				if !errors.Is(err, tt.want) {
					t.Fatalf("%v != %v", err, tt.want)
				}
			}
		})
	}
}

func TestStreamRejectsRegression(t *testing.T) {
	cases := []struct{ old, next Answer }{
		{Answer{Blocks: []Block{{Type: "text", Version: 1, Text: "ab"}}}, Answer{}},
		{Answer{Chips: []json.RawMessage{json.RawMessage(`{}`)}}, Answer{}},
		{Answer{Blocks: []Block{{Type: "text", Version: 1, Text: "ab"}}}, Answer{Blocks: []Block{{Type: "rows", Version: 1}}}},
		{Answer{Blocks: []Block{{Type: "text", Version: 1, Text: "ab"}}}, Answer{Blocks: []Block{{Type: "text", Version: 1, Text: "a"}}}},
		{Answer{Blocks: []Block{{Type: "rows", Version: 1, Body: json.RawMessage(`{}`)}}}, Answer{Blocks: []Block{{Type: "rows", Version: 1, Body: json.RawMessage(`{"changed":true}`)}}}},
		{Answer{Source: &Source{}}, Answer{}},
		{Answer{Chips: []json.RawMessage{json.RawMessage(`{}`)}}, Answer{Chips: []json.RawMessage{json.RawMessage(`{"changed":true}`)}}},
	}
	for _, tt := range cases {
		if _, err := changes(tt.old, tt.next); err == nil {
			t.Fatalf("regression allowed: %+v %+v", tt.old, tt.next)
		}
	}
	events, err := changes(Answer{}, Answer{Blocks: []Block{{Type: "text", Version: 1, Text: "hi"}}})
	if err != nil || len(events) != 1 || events[0].Text != "hi" {
		t.Fatalf("%+v %v", events, err)
	}
	events, err = changes(Answer{}, Answer{})
	if err != nil || len(events) != 0 {
		t.Fatal(err)
	}
}

func TestWatchPreservesEnrichedSource(t *testing.T) {
	at := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	later := at.Add(time.Second)
	a := Answer{Status: "generating", Source: &Source{CalculatedAt: at}, Blocks: []Block{{Type: "text", Version: 1, Text: "a"}}}
	b := a
	b.Status = "complete"
	b.Source = &Source{CalculatedAt: at}
	b.Blocks = []Block{{Type: "text", Version: 1, Text: "ab"}}
	n := &notices{ch: make(chan struct{}, 1)}
	v := &versions{version: Version{Uploads: &later}}
	count := 0
	err := New(&saved{answers: []Answer{a, b}}, v, n, allowance{}).Watch(t.Context(), "u", "a", func(e Event) error {
		count++
		if e.Kind == "snapshot" {
			n.ch <- struct{}{}
		}
		if e.Kind == "source" {
			t.Fatal("unchanged source emitted")
		}
		return nil
	})
	if err != nil || v.calls != 1 || count != 3 {
		t.Fatalf("%v calls=%d count=%d", err, v.calls, count)
	}
}
func TestFutureTextBlockUsesFallbackEvent(t *testing.T) {
	events, err := changes(Answer{}, Answer{Blocks: []Block{{Type: "text", Version: 2, Text: "not the fallback", Body: json.RawMessage(`{"alt_text":"fallback"}`)}}})
	if err != nil || len(events) != 1 || events[0].Kind != "block" || events[0].Block == nil {
		t.Fatalf("%+v %v", events, err)
	}
}

type snapshotHook struct {
	*saved
	first func()
}

func (r snapshotHook) Snapshot(ctx context.Context, user, id string) (Answer, error) {
	if r.saved.calls == 0 {
		r.first()
	}
	return r.saved.Snapshot(ctx, user, id)
}

type subscribeHook struct {
	*notices
	subscribed *bool
}

func (n subscribeHook) Subscribe(ctx context.Context, id string) (<-chan struct{}, func(), error) {
	*n.subscribed = true
	return n.notices.Subscribe(ctx, id)
}

func TestWatchSubscribesBeforeInitialRead(t *testing.T) {
	subscribed := false
	n := &notices{ch: make(chan struct{}, 1)}
	repo := snapshotHook{saved: &saved{answers: []Answer{
		{Status: "generating", Blocks: []Block{{Type: "text", Version: 1, Text: "before"}}},
		{Status: "complete", Blocks: []Block{{Type: "text", Version: 1, Text: "before and after"}}},
	}}, first: func() {
		if !subscribed {
			t.Fatal("initial snapshot read before subscription: concurrent commit would be lost")
		}
		// Model a commit after the repository took its initial snapshot, but before
		// that snapshot has returned to Watch or been sent to the client.
		n.ch <- struct{}{}
	}}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	var got []Event
	err := New(repo, &versions{}, subscribeHook{n, &subscribed}, allowance{}).Watch(ctx, "u", "a", func(e Event) error {
		got = append(got, e)
		return nil
	})
	if err != nil || len(got) != 3 || got[0].Kind != "snapshot" || got[0].Answer.Blocks[0].Text != "before" || got[1].Kind != "chunk" || got[1].Text != " and after" || got[2].Kind != "done" || !n.cancelled {
		t.Fatalf("events=%+v error=%v cancelled=%v", got, err, n.cancelled)
	}
}
