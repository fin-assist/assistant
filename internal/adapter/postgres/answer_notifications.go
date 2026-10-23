package postgres

import (
	"context"
	"encoding/hex"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/fin-assist/assistant/internal/dialogue"
)

type answerSubscription struct {
	ch   chan struct{}
	stop func() bool
}

const userSubscriptionPrefix = "user:"

// AnswerNotifications shares one primary LISTEN connection among all answer streams.
type AnswerNotifications struct {
	db          *DB
	ctx         context.Context
	cancel      context.CancelFunc
	done        chan struct{}
	mu          sync.Mutex
	ready       bool
	subscribers map[string]map[*answerSubscription]struct{}
}

// NewAnswerNotifications establishes LISTEN before admitting any subscribers.
// ctx is the application lifecycle, rather than an initiating RPC context.
func NewAnswerNotifications(ctx context.Context, db *DB) (*AnswerNotifications, error) {
	life, cancel := context.WithCancel(ctx)
	n := &AnswerNotifications{db: db, ctx: life, cancel: cancel, done: make(chan struct{}), subscribers: make(map[string]map[*answerSubscription]struct{})}
	conn, err := n.connect()
	if err != nil {
		cancel()
		return nil, dialogue.ErrUnavailable
	}
	n.ready = true
	go n.run(conn)
	return n, nil
}

func (n *AnswerNotifications) connect() (*pgx.Conn, error) {
	ctx, cancel := context.WithTimeout(n.ctx, 5*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, n.db.primary.Config().ConnConfig.Copy())
	if err != nil {
		return nil, err
	}
	if _, err = conn.Exec(ctx, `LISTEN assistant_answers; LISTEN assistant_users`); err != nil {
		closeAnswerConnection(conn)
		return nil, err
	}
	return conn, nil
}

func closeAnswerConnection(conn *pgx.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = conn.Close(ctx)
}

// Subscribe observes committed updates; closed updates means the transport was lost.
// The caller subscribes before reading its first primary snapshot to close the read/listen gap.
func (n *AnswerNotifications) Subscribe(ctx context.Context, answerID string) (<-chan struct{}, func(), error) {
	return n.subscribe(ctx, "", answerID)
}

// SubscribeUser observes this user's committed deletion notifications on the same
// LISTEN connection. A closed channel means observation was lost; callers must fail
// closed. Subscribe before checking the primary tombstone guard to avoid a gap.
func (n *AnswerNotifications) SubscribeUser(ctx context.Context, userID string) (<-chan struct{}, func(), error) {
	return n.subscribe(ctx, userSubscriptionPrefix, userID)
}

func (n *AnswerNotifications) subscribe(ctx context.Context, prefix, identifier string) (<-chan struct{}, func(), error) {
	id, valid := answerNotificationID(identifier)
	if !valid {
		return nil, nil, dialogue.ErrUnavailable
	}
	id = prefix + id
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if !n.ready || n.ctx.Err() != nil {
		return nil, nil, dialogue.ErrUnavailable
	}
	s := &answerSubscription{ch: make(chan struct{}, 1)}
	if n.subscribers[id] == nil {
		n.subscribers[id] = make(map[*answerSubscription]struct{})
	}
	n.subscribers[id][s] = struct{}{}
	cancel := func() { n.mu.Lock(); defer n.mu.Unlock(); n.remove(id, s) }
	s.stop = context.AfterFunc(ctx, cancel)
	return s.ch, cancel, nil
}

func (n *AnswerNotifications) remove(id string, s *answerSubscription) {
	group := n.subscribers[id]
	if _, ok := group[s]; !ok {
		return
	}
	delete(group, s)
	if len(group) == 0 {
		delete(n.subscribers, id)
	}
	if s.stop != nil {
		s.stop()
	}
	close(s.ch)
}

func (n *AnswerNotifications) unavailable() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.ready = false
	for id, group := range n.subscribers {
		for s := range group {
			n.remove(id, s)
		}
	}
}

func (n *AnswerNotifications) publish(payload string) {
	id, valid := answerNotificationID(payload)
	if !valid {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	for s := range n.subscribers[id] {
		select {
		case s.ch <- struct{}{}:
		default:
		}
	}
}

func (n *AnswerNotifications) publishAll(payload string) {
	id, valid := answerNotificationID(payload)
	if !valid {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	for key, group := range n.subscribers {
		// Answer watches reread their own snapshots on any deletion. Generation
		// cancellation is user-specific and must never stop a foreign user's task.
		if strings.HasPrefix(key, userSubscriptionPrefix) && key != userSubscriptionPrefix+id {
			continue
		}
		for s := range group {
			select {
			case s.ch <- struct{}{}:
			default:
			}
		}
	}
}

func (n *AnswerNotifications) run(conn *pgx.Conn) {
	defer close(n.done)
	defer n.unavailable()
	for {
		notification, err := conn.WaitForNotification(n.ctx)
		if err == nil {
			if notification.Channel == "assistant_users" {
				n.publishAll(notification.Payload)
			} else {
				n.publish(notification.Payload)
			}
			continue
		}
		n.unavailable()
		closeAnswerConnection(conn)
		for {
			if n.ctx.Err() != nil {
				return
			}
			timer := time.NewTimer(5 * time.Second)
			select {
			case <-n.ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			conn, err = n.connect()
			if err != nil {
				continue
			}
			n.mu.Lock()
			n.ready = true
			n.mu.Unlock()
			break
		}
	}
}

// Close stops observation and releases its dedicated connection. It is safe to repeat.
func (n *AnswerNotifications) Close() { n.cancel(); <-n.done }

func answerNotificationID(s string) (string, bool) {
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return "", false
	}
	compact := strings.ReplaceAll(s, "-", "")
	if len(compact) != 32 {
		return "", false
	}
	var raw [16]byte
	if _, err := hex.Decode(raw[:], []byte(compact)); err != nil {
		return "", false
	}
	return strings.ToLower(s), true
}
