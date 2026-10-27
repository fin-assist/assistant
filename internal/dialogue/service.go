package dialogue

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"

	"github.com/fin-assist/assistant/internal/datarevision"
)

// Repository supplies coherent, primary snapshots scoped by the verified user.
type Repository interface {
	List(context.Context, string, string, int) (Page, error)
	Snapshot(context.Context, string, string) (Answer, error)
}

// Versions reads the current primary source revisions from statement data.
type Versions interface {
	Get(context.Context, string) (Version, error)
}

// Notifications subscribes before the initial snapshot so a commit cannot fall in a gap.
type Notifications interface {
	Subscribe(context.Context, string) (<-chan struct{}, func(), error)
}

// Allowances supplies the current remaining daily allowance for terminal events.
type Allowances interface {
	Remaining(context.Context, string) (int, error)
}

// Event is a transport-independent saved-answer update.
type Event struct {
	Kind      string
	Index     int
	Text      string
	Block     *Block
	Chip      json.RawMessage
	Source    *Source
	Answer    *Answer
	Remaining int
}

// Service reads history and derives stream events from durable snapshots.
type Service struct {
	repo          Repository
	versions      Versions
	notifications Notifications
	allowance     Allowances
}

// New wires the consumer interfaces. The assembly owns their lifecycles.
func New(repo Repository, versions Versions, notifications Notifications, allowance Allowances) *Service {
	return &Service{repo: repo, versions: versions, notifications: notifications, allowance: allowance}
}

// List returns a page and marks stale sources with at most one peer call per page.
func (s *Service) List(ctx context.Context, user, before string, limit int) (Page, error) {
	page, err := s.repo.List(ctx, user, before, limit)
	if err != nil {
		return Page{}, err
	}
	hasSource := false
	for _, m := range page.Messages {
		if m.Answer != nil && m.Answer.Source != nil {
			hasSource = true
			break
		}
	}
	if !hasSource {
		return page, nil
	}
	version, err := s.currentVersion(ctx, user)
	if err != nil {
		return Page{}, err
	}
	for i := range page.Messages {
		if a := page.Messages[i].Answer; a != nil && a.Source != nil {
			a.Source = enrich(a.Source, version)
		}
	}
	return page, nil
}

func enrich(src *Source, version Version) *Source {
	result := *src
	result.ChangedSince = datarevision.Changes(src.Revisions, version.Revisions)
	return &result
}

// Watch emits a snapshot first, follows committed changes and ends on a terminal state.
// A failed notification transport ends the RPC; it never turns the saved answer into an error.
func (s *Service) Watch(ctx context.Context, user, id string, send func(Event) error) error {
	updates, cancel, err := s.notifications.Subscribe(ctx, id)
	if err != nil {
		return err
	}
	defer cancel()
	previous, err := s.repo.Snapshot(ctx, user, id)
	if err != nil {
		return err
	}
	if previous.Source != nil {
		version, err := s.currentVersion(ctx, user)
		if err != nil {
			return err
		}
		previous.Source = enrich(previous.Source, version)
	}
	if err := send(Event{Kind: "snapshot", Answer: &previous}); err != nil {
		return err
	}
	if previous.Status != "generating" {
		return s.terminal(ctx, user, previous, send)
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case _, ok := <-updates:
			if !ok {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return ErrUnavailable
			}
			next, err := s.repo.Snapshot(ctx, user, id)
			if err != nil {
				return err
			}
			// A failure discards the partial response. The terminal error is enough for the
			// client to replace it; never emit fragments from the failed snapshot.
			if next.Status == "failed" {
				return s.terminal(ctx, user, next, send)
			}
			if next.Status != "generating" && next.Status != "complete" {
				return errors.New("invalid saved answer status")
			}
			if next.Source != nil {
				version, err := s.currentVersion(ctx, user)
				if err != nil {
					return err
				}
				next.Source = enrich(next.Source, version)
			}

			events, err := changes(previous, next)
			if err != nil {
				return err
			}
			for _, event := range events {
				a := next
				event.Answer = &a
				if err := send(event); err != nil {
					return err
				}
			}
			if next.Status == "complete" {
				return s.terminal(ctx, user, next, send)
			}
			previous = next
		}
	}
}

// currentVersion degrades only an explicitly classified transient outage. Saved
// provenance stays immutable; all other peer errors and parent cancellation fail.
func (s *Service) currentVersion(ctx context.Context, user string) (Version, error) {
	version, err := s.versions.Get(ctx, user)
	if ctx.Err() != nil {
		return Version{}, ctx.Err()
	}
	if errors.Is(err, ErrVersionUnavailable) {
		return Version{}, nil
	}
	return version, err
}

func (s *Service) terminal(ctx context.Context, user string, a Answer, send func(Event) error) error {
	if a.Status != "complete" && a.Status != "failed" {
		return errors.New("invalid saved answer status")
	}
	remaining, err := s.allowance.Remaining(ctx, user)
	if err != nil {
		return err
	}
	kind := "done"
	if a.Status == "failed" {
		kind = "error"
	}
	return send(Event{Kind: kind, Answer: &a, Remaining: remaining})
}

func changes(old, next Answer) ([]Event, error) {
	if len(next.Blocks) < len(old.Blocks) || len(next.Chips) < len(old.Chips) {
		return nil, errors.New("saved answer regressed")
	}
	var events []Event
	for i, b := range next.Blocks {
		if i >= len(old.Blocks) {
			if b.Type == "text" && b.Version == 1 {
				events = append(events, Event{Kind: "chunk", Index: i, Text: b.Text})
			} else {
				b := b
				events = append(events, Event{Kind: "block", Index: i, Block: &b})
			}
			continue
		}
		prev := old.Blocks[i]
		if b.Type != prev.Type || b.Version != prev.Version {
			return nil, errors.New("saved block changed type")
		}
		if b.Type == "text" && b.Version == 1 {
			if !strings.HasPrefix(b.Text, prev.Text) {
				return nil, errors.New("saved text regressed")
			}
			if text := strings.TrimPrefix(b.Text, prev.Text); text != "" {
				events = append(events, Event{Kind: "chunk", Index: i, Text: text})
			}
		} else if !reflect.DeepEqual(b, prev) {
			return nil, errors.New("saved nontext block changed")
		}
	}
	if !reflect.DeepEqual(old.Source, next.Source) {
		if next.Source == nil {
			return nil, errors.New("saved source disappeared")
		}
		events = append(events, Event{Kind: "source", Source: next.Source})
	}
	for i, c := range next.Chips {
		if i < len(old.Chips) {
			if !reflect.DeepEqual(c, old.Chips[i]) {
				return nil, errors.New("saved chip changed")
			}
			continue
		}
		events = append(events, Event{Kind: "chip", Chip: c})
	}
	return events, nil
}
