package postgres

import (
	"reflect"
	"testing"

	"github.com/fin-assist/assistant/internal/datarevision"
	"github.com/fin-assist/assistant/internal/dialogue"
	"github.com/fin-assist/assistant/internal/generation"
)

func TestIntegrationSourceRevisionStorageRoundTripAndLegacy(t *testing.T) {
	db := dialogueDB(t)
	task := acceptedQuestion(t, db, dialogueUser, "source", storeNow)
	revisions := datarevision.Baseline{Values: map[string][]int64{"categories": {0, 9223372036854775807}, "uploads": {11, 12}, "time_zone": {3}, "future": {9}}, Incomplete: true}
	out := generation.Outcome{Blocks: []dialogue.Block{{Type: "text", Version: 1, Text: "answer"}}, Source: &dialogue.Source{CalculatedAt: storeNow, Revisions: revisions}}
	if !finishStored(t, db, task, out, storeNow) {
		t.Fatal("not stored")
	}
	got, err := NewDialogue(db).Snapshot(t.Context(), dialogueUser, task.AnswerID)
	if err != nil || got.Source == nil || !reflect.DeepEqual(got.Source.Revisions, revisions) {
		t.Fatal(got, err)
	}
	page, err := NewDialogue(db).List(t.Context(), dialogueUser, "", 20)
	if err != nil || len(page.Messages) != 2 || !reflect.DeepEqual(page.Messages[1].Answer.Source.Revisions, revisions) {
		t.Fatal(page, err)
	}
	dialogueExec(t, db, `UPDATE answer SET data_revisions=NULL WHERE user_id=$1`, dialogueUser)
	legacy, err := NewDialogue(db).Snapshot(t.Context(), dialogueUser, task.AnswerID)
	if err != nil || legacy.Source == nil || len(datarevision.Changes(legacy.Source.Revisions, datarevision.Current{"categories": 0, "uploads": 0, "time_zone": 0})) != 1 {
		t.Fatal(legacy, err)
	}
	for _, bad := range []string{`{"values":{"PRIVATEbad":[1]}}`, `{"values":{"uploads":[-1]}}`, `{"values":{"uploads":[1,1]}}`, `{"values":{"uploads":[]}}`, `[]`, `{"values":{"categories":[null],"uploads":[null],"time_zone":[null]},"incomplete":false}`, `{"values":{"categories":[0],"uploads":[0],"time_zone":[0]},"incomplete":null}`, `{"VALUES":{"categories":[0],"uploads":[0],"time_zone":[0]},"incomplete":false}`} {
		dialogueExec(t, db, `UPDATE answer SET data_revisions=$2 WHERE user_id=$1`, dialogueUser, bad)
		got, err := NewDialogue(db).Snapshot(t.Context(), dialogueUser, task.AnswerID)
		if err != nil || got.Source == nil || !reflect.DeepEqual(datarevision.Changes(got.Source.Revisions, datarevision.Current{"categories": 0, "uploads": 0, "time_zone": 0}), []string{"unverified"}) {
			t.Fatal(got, err)
		}
	}
}
