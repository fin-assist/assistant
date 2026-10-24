package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fin-assist/assistant/internal/dialogue"
)

func isolatedUserNotifications() *AnswerNotifications {
	return &AnswerNotifications{ctx: context.Background(), ready: true, subscribers: make(map[string]map[*answerSubscription]struct{})}
}

func TestUnitUserNotificationsKeepAnswerAndUserNamespacesSeparate(t *testing.T) {
	n := isolatedUserNotifications()
	const user = "abcdefab-0000-0000-0000-000000000001"
	own, stopOwn, err := n.SubscribeUser(t.Context(), strings.ToUpper(user))
	if err != nil {
		t.Fatal(err)
	}
	defer stopOwn()
	foreign, stopForeign, err := n.SubscribeUser(t.Context(), dialogueForeign)
	if err != nil {
		t.Fatal(err)
	}
	defer stopForeign()
	answerSameID, stopAnswer, err := n.Subscribe(t.Context(), user)
	if err != nil {
		t.Fatal(err)
	}
	defer stopAnswer()
	otherAnswer, stopOther, err := n.Subscribe(t.Context(), dialogueAnswer)
	if err != nil {
		t.Fatal(err)
	}
	defer stopOther()
	n.publish(strings.ToUpper(user))
	nextAnswerSignal(t, answerSameID, false)
	noAnswerSignal(t, own)
	noAnswerSignal(t, foreign)
	noAnswerSignal(t, otherAnswer)
	n.publishAll(dialogueForeign)
	nextAnswerSignal(t, foreign, false)
	nextAnswerSignal(t, answerSameID, false)
	nextAnswerSignal(t, otherAnswer, false)
	noAnswerSignal(t, own)
	n.publishAll("invalid private payload")
	noAnswerSignal(t, own)
	noAnswerSignal(t, foreign)
	noAnswerSignal(t, answerSameID)
	n.publishAll(strings.ToUpper(user))
	n.publishAll(user)
	nextAnswerSignal(t, own, false)
	noAnswerSignal(t, own)
	nextAnswerSignal(t, answerSameID, false)
	noAnswerSignal(t, answerSameID)
	nextAnswerSignal(t, otherAnswer, false)
	noAnswerSignal(t, foreign)
	stopOwn()
	nextAnswerSignal(t, own, true)
	stopOwn()
	n.publishAll(user)
	noAnswerSignal(t, foreign)
}

func TestUnitUserNotificationsSubscribeValidationCancellationAndUnavailable(t *testing.T) {
	n := isolatedUserNotifications()
	for _, id := range []string{"", "not UUID", "10000000-0000-0000-0000-00000000000z"} {
		ch, cancel, err := n.SubscribeUser(t.Context(), id)
		if ch != nil || cancel != nil || !errors.Is(err, dialogue.ErrUnavailable) {
			t.Fatalf("invalid subscription: %v", err)
		}
	}
	cancelled, stop := context.WithCancel(t.Context())
	stop()
	if _, _, err := n.SubscribeUser(cancelled, dialogueUser); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled subscription: %v", err)
	}
	ctx, stop := context.WithCancel(t.Context())
	ch, _, err := n.SubscribeUser(ctx, dialogueUser)
	if err != nil {
		t.Fatal(err)
	}
	stop()
	nextAnswerSignal(t, ch, true)
	ch, _, err = n.SubscribeUser(t.Context(), dialogueUser)
	if err != nil {
		t.Fatal(err)
	}
	answers, _, err := n.Subscribe(t.Context(), dialogueAnswer)
	if err != nil {
		t.Fatal(err)
	}
	n.unavailable()
	nextAnswerSignal(t, ch, true)
	nextAnswerSignal(t, answers, true)
	if _, _, err := n.SubscribeUser(t.Context(), dialogueUser); !errors.Is(err, dialogue.ErrUnavailable) {
		t.Fatalf("disconnected subscription: %v", err)
	}
	n.ready = true
	life, stop := context.WithCancel(t.Context())
	stop()
	n.ctx = life
	if _, _, err := n.SubscribeUser(t.Context(), dialogueUser); !errors.Is(err, dialogue.ErrUnavailable) {
		t.Fatalf("closed lifecycle subscription: %v", err)
	}
}

func TestIntegrationUserNotificationsAcrossInstancesRespectTransactionsAndScope(t *testing.T) {
	db := dialogueDB(t)
	var instances []*AnswerNotifications
	var own, foreign, answers []<-chan struct{}
	for range 2 {
		n, err := NewAnswerNotifications(t.Context(), db)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(n.Close)
		instances = append(instances, n)
		ch, cancel, err := n.SubscribeUser(t.Context(), dialogueUser)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(cancel)
		own = append(own, ch)
		ch, cancel, err = n.SubscribeUser(t.Context(), dialogueForeign)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(cancel)
		foreign = append(foreign, ch)
		ch, cancel, err = n.Subscribe(t.Context(), dialogueAnswer)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(cancel)
		answers = append(answers, ch)
	}
	// A committed answer update is an ordered barrier in both listeners. Neither
	// a rolled-back deletion nor an answer update may cancel a user's generation.
	tx, err := db.primary.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `SELECT pg_notify('assistant_users',$1)`, dialogueUser); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	dialogueExec(t, db, `SELECT pg_notify('assistant_answers',$1)`, dialogueAnswer)
	for i := range instances {
		nextAnswerSignal(t, answers[i], false)
		noAnswerSignal(t, own[i])
		noAnswerSignal(t, foreign[i])
	}
	tx, err = db.primary.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(t.Context(), `SELECT pg_notify('assistant_users',$1)`, dialogueUser); err != nil {
		t.Fatal(err)
	}
	// The following answer notification commits first. It proves both listeners
	// have advanced while the deletion transaction remains uncommitted.
	dialogueExec(t, db, `SELECT pg_notify('assistant_answers',$1)`, dialogueAnswer)
	for i := range instances {
		nextAnswerSignal(t, answers[i], false)
		noAnswerSignal(t, own[i])
		noAnswerSignal(t, foreign[i])
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	for i := range instances {
		nextAnswerSignal(t, own[i], false)
		nextAnswerSignal(t, answers[i], false)
		noAnswerSignal(t, foreign[i])
	}
	dialogueExec(t, db, `SELECT pg_notify('assistant_users',$1)`, dialogueForeign)
	for i := range instances {
		nextAnswerSignal(t, foreign[i], false)
		nextAnswerSignal(t, answers[i], false)
		noAnswerSignal(t, own[i])
	}
}

func TestIntegrationUserNotificationsDisconnectCancelsAndReconnects(t *testing.T) {
	db := dialogueDB(t)
	n, err := NewAnswerNotifications(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(n.Close)
	ch, _, err := n.SubscribeUser(t.Context(), dialogueUser)
	if err != nil {
		t.Fatal(err)
	}
	dialogueExec(t, db, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid() AND query LIKE 'LISTEN assistant_answers%'`)
	nextAnswerSignal(t, ch, true)
	if _, _, err := n.SubscribeUser(t.Context(), dialogueUser); !errors.Is(err, dialogue.ErrUnavailable) {
		t.Fatalf("disconnected user subscription: %v", err)
	}
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-deadline.C:
			t.Fatal("user observation did not recover")
		case <-tick.C:
			recovered, cancel, err := n.SubscribeUser(t.Context(), dialogueUser)
			if err != nil {
				continue
			}
			defer cancel()
			dialogueExec(t, db, `SELECT pg_notify('assistant_users',$1)`, dialogueUser)
			nextAnswerSignal(t, recovered, false)
			n.Close()
			nextAnswerSignal(t, recovered, true)
			if _, _, err := n.SubscribeUser(t.Context(), dialogueUser); !errors.Is(err, dialogue.ErrUnavailable) {
				t.Fatalf("closed user subscription: %v", err)
			}
			return
		}
	}
}
