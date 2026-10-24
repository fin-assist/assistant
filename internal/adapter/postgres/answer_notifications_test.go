package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fin-assist/assistant/internal/dialogue"
)

func nextAnswerSignal(t *testing.T, ch <-chan struct{}, closed bool) {
	t.Helper()
	select {
	case _, ok := <-ch:
		if ok == closed {
			t.Fatalf("channel open=%v, expected closed=%v", ok, closed)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("notification timeout")
	}
}
func noAnswerSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
		t.Fatal("unexpected notification")
	default:
	}
}
func TestIntegrationAnswerNotificationsCommitAndAllObservers(t *testing.T) {
	db := dialogueDB(t)
	seedDialogue(t, db)
	n, err := NewAnswerNotifications(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(n.Close)
	one, cancel, err := n.Subscribe(t.Context(), dialogueAnswer)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	two, cancel2, err := n.Subscribe(t.Context(), dialogueAnswer)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel2()
	other, cancel3, err := n.Subscribe(t.Context(), dialogueQuestion)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel3()
	// LISTEN is established before subscription, then durable state changes and its notification commit together.
	tx, err := db.primary.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(t.Context(), `UPDATE answer_block SET markdown='committed' WHERE user_id=$1 AND answer_id=$2 AND position=0`, dialogueUser, dialogueAnswer); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `SELECT pg_notify('assistant_answers',$1)`, dialogueAnswer); err != nil {
		t.Fatal(err)
	}
	noAnswerSignal(t, one)
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	// A first snapshot racing an update is covered either by the snapshot or its queued wake-up.
	a, err := NewDialogue(db).Snapshot(t.Context(), dialogueUser, dialogueAnswer)
	if err != nil || a.Blocks[0].Text != "committed" {
		t.Fatalf("snapshot: %v", err)
	}
	nextAnswerSignal(t, one, false)
	nextAnswerSignal(t, two, false)
	noAnswerSignal(t, other)
	dialogueExec(t, db, `SELECT pg_notify('assistant_users',$1)`, dialogueUser)
	nextAnswerSignal(t, one, false)
	nextAnswerSignal(t, two, false)
	nextAnswerSignal(t, other, false)
	cancel()
	nextAnswerSignal(t, one, true)
	cancel()
}
func TestIntegrationAnswerNotificationsCoalescingAndCleanup(t *testing.T) {
	db := dialogueDB(t)
	n, err := NewAnswerNotifications(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(n.Close)
	ctx, cancel := context.WithCancel(t.Context())
	ch, _, err := n.Subscribe(ctx, dialogueAnswer)
	if err != nil {
		t.Fatal(err)
	}
	// Unit-level delivery tests the bounded mailbox independently of database notification coalescing.
	n.publish(dialogueAnswer)
	n.publish(dialogueAnswer)
	n.publish("bad payload")
	n.publish(dialogueQuestion)
	nextAnswerSignal(t, ch, false)
	noAnswerSignal(t, ch)
	cancel()
	nextAnswerSignal(t, ch, true)
	if _, _, err := n.Subscribe(ctx, dialogueAnswer); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
	if _, _, err := n.Subscribe(t.Context(), "not UUID"); !errors.Is(err, dialogue.ErrUnavailable) {
		t.Fatalf("invalid: %v", err)
	}
	ch, _, err = n.Subscribe(t.Context(), dialogueAnswer)
	if err != nil {
		t.Fatal(err)
	}
	n.Close()
	nextAnswerSignal(t, ch, true)
	n.Close()
	if _, _, err := n.Subscribe(t.Context(), dialogueAnswer); !errors.Is(err, dialogue.ErrUnavailable) {
		t.Fatalf("closed: %v", err)
	}
}
func TestIntegrationAnswerNotificationsDisconnectClosesAndReconnects(t *testing.T) {
	db := dialogueDB(t)
	n, err := NewAnswerNotifications(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(n.Close)
	ch, _, err := n.Subscribe(t.Context(), dialogueAnswer)
	if err != nil {
		t.Fatal(err)
	}
	dialogueExec(t, db, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid() AND query LIKE 'LISTEN assistant_answers%'`)
	nextAnswerSignal(t, ch, true)
	if _, _, err := n.Subscribe(t.Context(), dialogueAnswer); !errors.Is(err, dialogue.ErrUnavailable) {
		t.Fatalf("disconnected: %v", err)
	}
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-deadline.C:
			t.Fatal("reconnect timeout")
		case <-tick.C:
			ch, cancel, err := n.Subscribe(t.Context(), dialogueAnswer)
			if err != nil {
				continue
			}
			defer cancel()
			dialogueExec(t, db, `SELECT pg_notify('assistant_answers',$1)`, dialogueAnswer)
			nextAnswerSignal(t, ch, false)
			return
		}
	}
}
func TestUnitAnswerNotificationID(t *testing.T) {
	for _, id := range []string{"", "x", "10000000_0000-0000-0000-000000000001", "10000000-0000-0000-0000-00000000000z"} {
		if _, ok := answerNotificationID(id); ok {
			t.Fatalf("accepted %q", id)
		}
	}
	if id, ok := answerNotificationID("ABCDEFAB-0000-0000-0000-000000000001"); !ok || id != "abcdefab-0000-0000-0000-000000000001" {
		t.Fatalf("canonical: %q %v", id, ok)
	}
}

func TestIntegrationAnswerNotificationsCancelledLifecycle(t *testing.T) {
	db := dialogueDB(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if n, err := NewAnswerNotifications(ctx, db); n != nil || !errors.Is(err, dialogue.ErrUnavailable) {
		t.Fatalf("cancelled constructor: %v", err)
	}
	ctx, cancel = context.WithCancel(t.Context())
	n, err := NewAnswerNotifications(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(n.Close)
	ch, _, err := n.Subscribe(t.Context(), dialogueAnswer)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	nextAnswerSignal(t, ch, true)
	n.Close()
}
