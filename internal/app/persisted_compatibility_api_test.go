package app_test

import (
	"fmt"
	"math"
	"strings"
	"testing"

	assistantv1 "github.com/fin-assist/contracts/gen/go/finassist/assistant/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/fin-assist/assistant/internal/app/apitest"
)

func persistedAPIBlock(t *testing.T, env *apitest.Env, kind string, version int, body string) {
	t.Helper()
	dialogueAPIExec(t, env, `UPDATE answer_block SET type=$3,schema_version=$4,markdown=NULL,body=$5::jsonb WHERE user_id=$1 AND answer_id=$2`, apiDialogueUser, apiDialogueAnswer, kind, version, body)
}

// These fixtures represent persisted data from previous/future deployments, not
// output attributed to the current generation engine. Both public readers must
// preserve native numeric precision and the promised rolling-upgrade fallback.
func TestAPIPersistedCompatibilityHistoryAndWatchAgree(t *testing.T) {
	for _, tc := range []struct {
		name, kind, body string
		version          int
	}{
		{"exact-rows", "rows", `{"alt_text":"Сохранённые значения","data":{"rows":[{"label":"Сумма","value_kind":"amount","value":9223372036854775807},{"label":"Количество","value_kind":"count","value":9223372036854775807,"sublabel":"Точно"},{"label":"Доля","value_kind":"percent","value":0.5},{"label":"Текст","value_kind":"text","value":"Сохранено"},{"label":"Будущее значение","value_kind":"future","text":"Совместимый текст"}]}}`, 1},
		{"future-type", "future_widget", `{"alt_text":"Совместимое пояснение","data":{"PRIVATE":"unrecognized payload"}}`, 1},
		{"future-version", "rows", `{"alt_text":"Совместимое пояснение","data":{"PRIVATE":"unrecognized payload"}}`, 2},
		{"future-chart-kind", "chart", `{"alt_text":"Совместимое пояснение","data":{"kind":"line","value_kind":"amount","PRIVATE":"unrecognized payload"}}`, 1},
		{"future-chart-unit", "chart", `{"alt_text":"Совместимое пояснение","data":{"kind":"bar","value_kind":"future","PRIVATE":"unrecognized payload"}}`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := apitest.Start(t)
			seedAPIDialogue(t, env, "complete")
			persistedAPIBlock(t, env, tc.kind, tc.version, tc.body)
			dialogueAPIExec(t, env, `UPDATE answer SET chips='[{"label":"Операции","screen":"operations","filters":{"kind":"income","q":"Доход","amount_from":0,"amount_to":9223372036854775807,"transfer_mode":"with","selection":"salary","selection_name":"Зарплата"}},{"label":"Аналитика","screen":"analytics","params":{"period":"year","date":"2026","transfer_mode":"without"}}]'::jsonb WHERE user_id=$1 AND id=$2`, apiDialogueUser, apiDialogueAnswer)
			ctx, cancel := apiDialogueContext(t, apiDialogueUser)
			defer cancel()
			client := assistantv1.NewAssistantServiceClient(env.Conn)
			page, err := client.ListMessages(ctx, &assistantv1.ListMessagesRequest{})
			if err != nil || len(page.GetMessages()) != 2 {
				t.Fatalf("history: %v %v", page, err)
			}
			saved := page.Messages[1]
			stream, err := client.WatchAnswer(ctx, &assistantv1.WatchAnswerRequest{AnswerId: apiDialogueAnswer})
			if err != nil {
				t.Fatal(err)
			}
			event, err := stream.Recv()
			if err != nil || event.GetSnapshot() == nil {
				t.Fatalf("snapshot: %v %v", event, err)
			}
			snapshot := event.GetSnapshot()
			if len(saved.Blocks) != 1 || len(snapshot.Blocks) != 1 || !proto.Equal(saved.Blocks[0], snapshot.Blocks[0]) || len(saved.Chips) != 2 || len(snapshot.Chips) != 2 || !proto.Equal(saved.Chips[0], snapshot.Chips[0]) {
				t.Fatalf("reader disagreement: history=%v snapshot=%v", saved, snapshot)
			}
			if saved.Chips[0].Filters.GetAmountToMinor() != math.MaxInt64 || saved.Chips[0].Filters.GetSelectionName() != "Зарплата" || saved.Chips[1].Params.GetPeriodKey() != "2026" {
				t.Fatal("persisted navigation lost exact parameters")
			}
			if tc.name == "exact-rows" {
				rows := saved.Blocks[0].GetRows().GetRows()
				if len(rows) != 5 || rows[0].GetAmountMinor() != math.MaxInt64 || rows[1].GetCount() != math.MaxInt64 || rows[1].GetSublabel() != "Точно" || rows[2].GetPercent() != 0.5 || rows[3].GetText() != "Сохранено" || rows[4].GetText() != "Совместимый текст" {
					t.Fatalf("persisted native values: %v", rows)
				}
			} else if saved.Blocks[0].GetText().GetText() != "Совместимое пояснение" || strings.Contains(saved.String(), "PRIVATE") {
				t.Fatal("future payload did not use its safe alt_text")
			}
			terminal, err := stream.Recv()
			if err != nil || terminal.GetDone() == nil {
				t.Fatalf("terminal: %v %v", terminal, err)
			}
		})
	}
}

// PostgreSQL enforces relationships, but JSONB does not enforce catalog shape.
// Historical corruption must fail closed consistently at both public readers.
func TestAPIPersistedCorruptionFailsBothReadersWithoutLeakingPayload(t *testing.T) {
	for i, body := range []string{
		`{"PRIVATE":"secret"}`, `{"alt_text":1}`, `null`, `{"alt_text":"safe","data":{"rows":"PRIVATE"}}`,
		`{"alt_text":"safe","data":{"rows":[{"value_kind":"count","value":1}]}}`,
		`{"alt_text":"safe","data":{"rows":[{"label":"x","value_kind":"amount"}]}}`,
		`{"alt_text":"safe","data":{"rows":[{"label":"x","value_kind":"amount","value":"PRIVATE"}]}}`,
		`{"alt_text":"safe","data":{"rows":[{"label":"x","value_kind":"amount","value":null}]}}`,
		`{"alt_text":"safe","data":{"rows":[{"label":"x","value_kind":"amount","value":1.5}]}}`,
		`{"alt_text":"safe","data":{"rows":[{"label":"x","value_kind":"count","value":-1}]}}`,
		`{"alt_text":"safe","data":{"rows":[{"label":"x","value_kind":"amount","value":9223372036854775808}]}}`,
		`{"alt_text":"safe","data":{"rows":[{"label":"x","value_kind":"percent"}]}}`,
		`{"alt_text":"safe","data":{"rows":[{"label":"x","value_kind":"percent","value":"PRIVATE"}]}}`,
		`{"alt_text":"safe","data":{"rows":[{"label":"x","value_kind":"percent","value":2}]}}`,
		`{"alt_text":"safe","data":{"rows":[{"label":"x","value_kind":"text","value":null}]}}`,
		`{"alt_text":"safe","data":{"rows":[{"label":"x","value_kind":"future"}]}}`,
	} {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			env := apitest.Start(t)
			seedAPIDialogue(t, env, "complete")
			persistedAPIBlock(t, env, "rows", 1, body)
			persistedAPIAssertClosed(t, env)
		})
	}
}

func persistedAPIAssertClosed(t *testing.T, env *apitest.Env) {
	t.Helper()
	ctx, cancel := apiDialogueContext(t, apiDialogueUser)
	defer cancel()
	client := assistantv1.NewAssistantServiceClient(env.Conn)
	page, listErr := client.ListMessages(ctx, &assistantv1.ListMessagesRequest{})
	if page != nil || status.Code(listErr) != codes.Internal || strings.Contains(listErr.Error(), "PRIVATE") {
		t.Fatalf("corrupt history was not safely rejected: %v %v", page, listErr)
	}
	stream, watchErr := client.WatchAnswer(ctx, &assistantv1.WatchAnswerRequest{AnswerId: apiDialogueAnswer})
	if watchErr == nil {
		event, err := stream.Recv()
		watchErr = err
		if event != nil {
			t.Fatal("corrupt answer emitted a snapshot")
		}
	}
	if status.Code(watchErr) != codes.Internal || strings.Contains(watchErr.Error(), "PRIVATE") || status.Convert(listErr).Message() != status.Convert(watchErr).Message() {
		t.Fatalf("inconsistent corrupt-data rejection: list=%v watch=%v", listErr, watchErr)
	}
}

func TestAPIPersistedChartCorruptionAndNavigationFailClosed(t *testing.T) {
	chart := func(value, coverage string) string {
		return `{"alt_text":"Historical chart","data":{"kind":"bar","value_kind":"amount","points":[{"range":{"from":"2026-09-01T00:00:00Z","to":"2026-10-01T00:00:00Z"},"coverage":"` + coverage + `","value":` + value + `}]}}`
	}
	for name, body := range map[string]string{
		"chart-rounded-money":          chart("9007199254740992", "complete"),
		"chart-negative-rounded-money": chart("-9007199254740992", "complete"),
		"chart-fractional-money":       chart("1.5", "complete"),
		"chart-unknown-coverage":       chart("1", "future"),
		"chart-highlight-outside":      strings.Replace(chart("1", "complete"), `"points":`, `"highlight_index":1,"points":`, 1),
		"chart-reversed-period":        strings.Replace(chart("1", "complete"), "2026-10-01", "2026-08-01", 1),
		"chart-zero-date":              strings.Replace(chart("1", "complete"), "2026-09-01T00:00:00Z", "0001-01-01T00:00:00Z", 1),
		"chart-invalid-bounds":         strings.Replace(chart("1", "complete"), `"coverage":`, `"data_from":"0001-01-01T00:00:00Z","coverage":`, 1),
		"chart-invalid-end-bounds":     strings.Replace(chart("1", "complete"), `"coverage":`, `"data_to":"0001-01-01T00:00:00Z","coverage":`, 1),
		"chart-negative-percent":       strings.Replace(chart("-0.1", "complete"), `"value_kind":"amount"`, `"value_kind":"percent"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			env := apitest.Start(t)
			seedAPIDialogue(t, env, "complete")
			persistedAPIBlock(t, env, "chart", 1, body)
			persistedAPIAssertClosed(t, env)
		})
	}
	for i, chip := range []string{
		`null`, `{"label":"x","screen":"future"}`, `{"label":"x","screen":"operations"}`,
		`{"label":"x","screen":"operations","filters":{"q":"x","transfer_mode":"future"}}`,
		`{"label":"x","screen":"operations","filters":{"q":"x","transfer_mode":"with","kind":"future"}}`,
		`{"label":"x","screen":"operations","filters":{"amount_from":-1,"transfer_mode":"with"}}`,
		`{"label":"x","screen":"operations","filters":{"amount_from":10,"amount_to":1,"transfer_mode":"with"}}`,
		`{"label":"x","screen":"operations","filters":{"from":"2026-10-01T00:00:00Z","to":"2026-09-01T00:00:00Z","transfer_mode":"with"}}`,
		`{"label":"x","screen":"operations","filters":{"from":"0001-01-01T00:00:00Z","transfer_mode":"with"}}`,
		`{"label":"x","screen":"operations","filters":{"to":"0001-01-01T00:00:00Z","transfer_mode":"with"}}`,
		`{"label":"x","screen":"analytics"}`,
		`{"label":"x","screen":"analytics","params":{"period":"month","date":"2026-99","transfer_mode":"with"}}`,
		`{"label":"x","screen":"analytics","params":{"period":"quarter","date":"2026-Q5","transfer_mode":"with"}}`,
		`{"label":"x","screen":"analytics","params":{"period":"year","date":"26","transfer_mode":"with"}}`,
		`{"label":"x","screen":"analytics","params":{"period":"future","date":"2026","transfer_mode":"with"}}`,
	} {
		t.Run(fmt.Sprintf("chip-%d", i), func(t *testing.T) {
			env := apitest.Start(t)
			seedAPIDialogue(t, env, "complete")
			dialogueAPIExec(t, env, `UPDATE answer SET chips=$3::jsonb WHERE user_id=$1 AND id=$2`, apiDialogueUser, apiDialogueAnswer, "["+chip+"]")
			persistedAPIAssertClosed(t, env)
		})
	}
}

func TestAPIPersistedLegacyFailuresExposeServerOwnedPolicy(t *testing.T) {
	for _, tc := range []struct {
		stored string
		code   assistantv1.AnswerErrorCode
		retry  bool
	}{
		{"MODEL_ERROR", assistantv1.AnswerErrorCode_ANSWER_ERROR_CODE_MODEL_ERROR, true},
		{"CANNOT_ANSWER", assistantv1.AnswerErrorCode_ANSWER_ERROR_CODE_CANNOT_ANSWER, false},
		{"OFF_TOPIC", assistantv1.AnswerErrorCode_ANSWER_ERROR_CODE_OFF_TOPIC, false},
	} {
		t.Run(tc.stored, func(t *testing.T) {
			env := apitest.Start(t)
			seedAPIDialogue(t, env, "failed")
			dialogueAPIExec(t, env, `UPDATE answer SET error_code=$3,retryable=NULL,charged=false WHERE user_id=$1 AND id=$2`, apiDialogueUser, apiDialogueAnswer, tc.stored)
			ctx, cancel := apiDialogueContext(t, apiDialogueUser)
			defer cancel()
			client := assistantv1.NewAssistantServiceClient(env.Conn)
			page, err := client.ListMessages(ctx, &assistantv1.ListMessagesRequest{})
			if err != nil || len(page.GetMessages()) != 2 {
				t.Fatalf("legacy history: %v %v", page, err)
			}
			saved := page.Messages[1]
			if saved.ErrorCode != tc.code || saved.GetRetryable() != tc.retry || saved.GetErrorText() == "" || len(saved.Blocks) != 0 {
				t.Fatalf("legacy error policy: %v", saved)
			}
			stream, err := client.WatchAnswer(ctx, &assistantv1.WatchAnswerRequest{AnswerId: apiDialogueAnswer})
			if err != nil {
				t.Fatal(err)
			}
			if first, err := stream.Recv(); err != nil || first.GetSnapshot() == nil || len(first.GetSnapshot().Blocks) != 0 {
				t.Fatalf("failed snapshot: %v %v", first, err)
			}
			terminal, err := stream.Recv()
			failure := terminal.GetError()
			if err != nil || failure.GetCode() != tc.code || failure.GetRetryable() != tc.retry || failure.GetText() != saved.GetErrorText() {
				t.Fatalf("legacy terminal differs from history: %v %v", terminal, err)
			}
		})
	}
}
