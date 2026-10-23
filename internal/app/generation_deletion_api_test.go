package app_test

import (
	"net/http"
	"testing"
	"time"

	assistantv1 "github.com/fin-assist/contracts/gen/go/finassist/assistant/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	deletionv1 "github.com/fin-assist/contracts/gen/go/finassist/userdeletion/v1"

	"github.com/fin-assist/assistant/internal/app/apitest"
)

func TestGenerationAPIDeletionOnAnotherInstanceCancelsOnlyItsUser(t *testing.T) {
	cancelled := make(chan int, 2)
	model := generationAPIModel(t, func(n int, _ http.ResponseWriter, r *http.Request) { <-r.Context().Done(); cancelled <- n })
	env := generationAPIEnv(t, &generationAPIPeers{grant: true}, model)
	first := generationAPIAsk(t, env, generationAPIUser, generationAPIKey(), "Первый вопрос", commonv1.TransferMode_TRANSFER_MODE_WITH)
	generationAPIWaitArrival(t, model)
	second := generationAPIAsk(t, env, generationAPIOther, generationAPIKey(), "Второй вопрос", commonv1.TransferMode_TRANSFER_MODE_WITH)
	generationAPIWaitArrival(t, model)
	otherPod := dialogueReplica(t, env)
	purger := deletionv1.NewUserDeletionServiceClient(otherPod)
	if _, err := purger.PurgeUserData(apitest.Caller(t.Context(), "user"), &deletionv1.PurgeUserDataRequest{UserId: generationAPIUser}); err != nil {
		t.Fatal(err)
	}
	select {
	case n := <-cancelled:
		if n != 1 {
			t.Fatalf("cancelled model request %d, want deleted user's first request", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("remote deletion did not cancel the active HTTP model request")
	}
	select {
	case <-cancelled:
		t.Fatal("another user's generation cancelled")
	default:
	}
	c := assistantv1.NewAssistantServiceClient(env.Conn)
	stream, err := c.WatchAnswer(generationAPIContext(t, generationAPIOther, ""), &assistantv1.WatchAnswerRequest{AnswerId: second.AnswerId})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := stream.Recv()
	if err != nil || snapshot.GetSnapshot().GetStatus() != assistantv1.AnswerStatus_ANSWER_STATUS_GENERATING {
		t.Fatalf("other user's snapshot=%v err=%v", snapshot, err)
	}
	var rows, keys int
	if err := env.App.DB.Writer(t.Context()).QueryRow(t.Context(), `SELECT (SELECT count(*) FROM answer WHERE user_id=$1),(SELECT count(*) FROM idempotency_keys WHERE scope=$1)`, generationAPIUser).Scan(&rows, &keys); err != nil {
		t.Fatal(err)
	}
	if rows != 0 || keys != 0 || model.count.Load() != 2 {
		t.Fatalf("deleted answer=%s rows=%d keys=%d HTTP=%d", first.AnswerId, rows, keys, model.count.Load())
	}
	if _, err := purger.PurgeUserData(apitest.Caller(t.Context(), "user"), &deletionv1.PurgeUserDataRequest{UserId: generationAPIOther}); err != nil {
		t.Fatal(err)
	}
	select {
	case n := <-cancelled:
		if n != 2 {
			t.Fatalf("second cancelled request=%d", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second user deletion did not drain model")
	}
}
