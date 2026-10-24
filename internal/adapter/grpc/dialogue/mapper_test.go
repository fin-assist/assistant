package dialogue

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	assistantv1 "github.com/fin-assist/contracts/gen/go/finassist/assistant/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"

	domain "github.com/fin-assist/assistant/internal/dialogue"
)

func TestUnitMessage(t *testing.T) {
	now := time.Date(2026, 10, 23, 1, 2, 3, 0, time.UTC)
	question, err := Message(domain.Message{ID: "q", CreatedAt: now, Question: "private question"})
	if err != nil || question.GetText() != "private question" || question.Role != assistantv1.Role_ROLE_USER || question.Currency != "RUB" {
		t.Fatalf("question = %v, %v", question, err)
	}
	for _, status := range []string{"generating", "complete", "failed"} {
		t.Run(status, func(t *testing.T) {
			a := domain.Answer{Status: status, ErrorCode: "MODEL_ERROR", Charged: true, Blocks: []domain.Block{{Type: "text", Version: 1, Text: "answer"}}}
			m, e := Message(domain.Message{ID: "a", CreatedAt: now, Answer: &a})
			if e != nil {
				t.Fatal(e)
			}
			if m.Role != assistantv1.Role_ROLE_ASSISTANT || m.Currency != "RUB" {
				t.Fatal(m)
			}
			switch status {
			case "failed":
				if len(m.Blocks) != 0 || m.GetErrorText() == "" || !m.GetRetryable() {
					t.Fatal(m)
				}
			case "complete":
				if !m.GetCharged() || len(m.Blocks) != 1 {
					t.Fatal(m)
				}
			default:
				if m.Charged != nil || len(m.Blocks) != 1 {
					t.Fatal(m)
				}
			}
		})
	}
	if _, err = Message(domain.Message{}); err == nil {
		t.Fatal("zero timestamp accepted")
	}
	if _, err = AnswerSnapshot(domain.Answer{Status: "future"}); err == nil {
		t.Fatal("future status accepted")
	}
	if _, err = Message(domain.Message{CreatedAt: now, Answer: &domain.Answer{Status: "failed", ErrorCode: "future"}}); err == nil {
		t.Fatal("unknown failure accepted")
	}
}

func TestUnitBlocksRowsPrecisionAndKinds(t *testing.T) {
	raw := `{"alt_text":"figures","data":{"rows":[{"label":"amount","value_kind":"amount","value":9223372036854775807},{"label":"count","sublabel":"sub","value_kind":"count","value":9},{"label":"percent","value_kind":"percent","value":0.5},{"label":"text","text":"none"},{"label":"future","value_kind":"new","text":"fallback"}]}}`
	blocks, err := Blocks([]domain.Block{{Type: "rows", Version: 1, Body: json.RawMessage(raw)}})
	if err != nil {
		t.Fatal(err)
	}
	rows := blocks[0].GetRows().Rows
	if len(rows) != 5 || rows[0].GetAmountMinor() != 9223372036854775807 || rows[1].GetCount() != 9 || rows[1].GetSublabel() != "sub" || rows[2].GetPercent() != 0.5 || rows[3].GetText() != "none" || rows[4].GetText() != "fallback" {
		t.Fatal(rows)
	}
	for _, row := range []string{`{"label":"x","value_kind":"amount","value":1.1}`, `{"label":"x","value_kind":"amount","value":9223372036854775808}`, `{"label":"x","value_kind":"count","value":-1}`, `{"label":"x","value_kind":"percent","value":1.1}`, `{"label":"x","value_kind":"percent"}`, `{"label":"x","value_kind":"count"}`, `{"label":"x"}`, `{"value_kind":"count","value":1}`} {
		_, err := Blocks([]domain.Block{{Type: "rows", Version: 1, Body: json.RawMessage(`{"alt_text":"x","data":{"rows":[` + row + `]}}`)}})
		if err == nil {
			t.Errorf("accepted %s", row)
		}
	}
}

func chartJSON(value, kind, coverage string) string {
	return `{"title":"chart","alt_text":"chart alt","data":{"kind":"bar","value_kind":"` + kind + `","points":[{"range":{"from":"2026-09-01T00:00:00Z","to":"2026-10-01T00:00:00Z"},"value":` + value + `,"coverage":"` + coverage + `","data_from":"2026-09-01T00:00:00Z","data_to":"2026-09-25T21:40:00Z"}]}}`
}
func TestUnitBlocksChart(t *testing.T) {
	for _, tc := range []struct {
		value, kind, coverage string
		fail                  bool
	}{{"9007199254740991", "amount", "complete", false}, {"-9007199254740991", "amount", "partial", false}, {"null", "amount", "no_data", false}, {"0.3", "percent", "complete", false}, {"9007199254740992", "amount", "complete", true}, {"-9007199254740992", "amount", "complete", true}, {"1.5", "amount", "complete", true}, {"1e309", "percent", "complete", true}, {"-0.1", "percent", "complete", true}, {"1", "amount", "future", true}} {
		t.Run(tc.value+tc.kind+tc.coverage, func(t *testing.T) {
			blocks, err := Blocks([]domain.Block{{Type: "chart", Version: 1, Body: json.RawMessage(chartJSON(tc.value, tc.kind, tc.coverage))}})
			if (err != nil) != tc.fail {
				t.Fatalf("err=%v", err)
			}
			if tc.fail {
				return
			}
			chart := blocks[0].GetChart()
			if chart.Title != "chart" || chart.Points[0].Bounds.DataTo.AsTime().Hour() != 21 {
				t.Fatal(chart)
			}
			if (chart.Points[0].Value == nil) != (tc.value == "null") {
				t.Fatal("lost nullable value")
			}
		})
	}
	for _, replacement := range []struct{ old, new string }{{`"points":`, `"highlight_index":1,"points":`}, {`"2026-10-01T00:00:00Z"`, `"2026-08-01T00:00:00Z"`}, {`"2026-09-25T21:40:00Z"`, `"bad"`}} {
		raw := strings.Replace(chartJSON("1", "amount", "partial"), replacement.old, replacement.new, 1)
		if _, e := Blocks([]domain.Block{{Type: "chart", Version: 1, Body: json.RawMessage(raw)}}); e == nil {
			t.Fatal("invalid chart accepted")
		}
	}
}
func TestUnitBlocksFallbackAndSanitization(t *testing.T) {
	blocks, e := Blocks([]domain.Block{{Type: "text", Version: 1, Text: "text"}, {Type: "future", Version: 9, Body: json.RawMessage(`{"alt_text":"equivalent","data":{"secret":"hidden"}}`)}, {Type: "chart", Version: 1, Body: json.RawMessage(chartJSON("1", "future", "complete"))}})
	if e != nil || len(blocks) != 3 || blocks[0].GetText().Text != "text" || blocks[1].GetText().Text != "equivalent" || blocks[2].GetText().Text != "chart alt" {
		t.Fatalf("%v %v", blocks, e)
	}
	for _, raw := range []string{`{"private":"SECRET"}`, `{"alt_text":1}`, `{"alt_text":"x","data":{"rows":"SECRET"}}`, `null`, `{"alt_text":"x"`} {
		_, e := Blocks([]domain.Block{{Type: "rows", Version: 1, Body: json.RawMessage(raw)}})
		if e == nil || strings.Contains(e.Error(), "SECRET") {
			t.Fatalf("unsafe failure %v", e)
		}
	}
}

func TestUnitSource(t *testing.T) {
	if s, e := Source(nil); e != nil || s != nil {
		t.Fatal(s, e)
	}
	from := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 1, 0)
	s := domain.Source{CalculatedAt: to, From: &from, To: &to, TransferMode: "without", ChangedSince: []string{"categories", "uploads"}}
	out, e := Source(&s)
	if e != nil || out.TransferMode != commonv1.TransferMode_TRANSFER_MODE_WITHOUT || len(out.ChangedSince) != 2 || !out.DataRange.Start.AsTime().Equal(from) {
		t.Fatal(out, e)
	}
	for _, mutate := range []func(*domain.Source){func(s *domain.Source) { s.From = nil }, func(s *domain.Source) { s.To = s.From }, func(s *domain.Source) { s.TransferMode = "new" }, func(s *domain.Source) { s.ChangedSince = []string{"new"} }, func(s *domain.Source) { s.CalculatedAt = time.Time{} }} {
		copy := s
		mutate(&copy)
		if _, e := Source(&copy); e == nil {
			t.Fatal("invalid source accepted")
		}
	}
}

func TestUnitChips(t *testing.T) {
	raw := []json.RawMessage{json.RawMessage(`{"label":"operations","screen":"operations","filters":{"from":"2026-09-01T00:00:00Z","to":"2026-10-01T00:00:00Z","q":"x","category_id":"c","kind":"expense","amount_from":1,"amount_to":9223372036854775807,"transfer_mode":"with","selection":"s","selection_name":"name"}}`)}
	for _, p := range []string{`"period":"month","date":"2026-09"`, `"period":"quarter","date":"2026-Q3"`, `"period":"year","date":"2026"`} {
		raw = append(raw, json.RawMessage(`{"label":"analytics","screen":"analytics","params":{`+p+`,"transfer_mode":"without"}}`))
	}
	chips, e := Chips(raw)
	if e != nil || len(chips) != 4 {
		t.Fatal(chips, e)
	}
	f := chips[0].Filters
	if f.GetAmountToMinor() != 9223372036854775807 || f.Kind != commonv1.KindFilter_KIND_FILTER_EXPENSE || f.GetSelectionName() != "name" || f.GetQ() != "x" {
		t.Fatal(f)
	}
	if chips[2].Params.PeriodKey != "2026-Q3" {
		t.Fatal(chips[2])
	}
	for _, raw := range []string{`null`, `{`, `{"label":"x","screen":"future"}`, `{"label":"x","screen":"operations"}`, `{"label":"x","screen":"operations","filters":{"transfer_mode":"bad"}}`, `{"label":"x","screen":"operations","filters":{"transfer_mode":"with","kind":"future"}}`, `{"label":"x","screen":"operations","filters":{"transfer_mode":"with","amount_from":-1}}`, `{"label":"x","screen":"analytics","params":{"period":"month","date":"2026-99","transfer_mode":"with"}}`, `{"label":"x","screen":"analytics","params":{"period":"new","date":"2026","transfer_mode":"with"}}`} {
		if _, e := Chips([]json.RawMessage{json.RawMessage(raw)}); e == nil {
			t.Errorf("accepted %s", raw)
		}
	}
}

func TestUnitErrorDefaultsAndStoredPolicy(t *testing.T) {
	for _, code := range []string{"MODEL_ERROR", "CANNOT_ANSWER", "OFF_TOPIC"} {
		a := domain.Answer{Status: "failed", ErrorCode: code}
		out, e := Error(a, 5)
		if e != nil || out.Text == "" || out.RemainingLimit != 5 || out.Retryable != (code == "MODEL_ERROR") {
			t.Fatal(out, e)
		}
		retry := false
		a.Retryable = &retry
		out, e = Error(a, 0)
		if e != nil || out.Retryable {
			t.Fatal(out, e)
		}
	}
	for _, a := range []domain.Answer{{Status: "complete", ErrorCode: "MODEL_ERROR"}, {Status: "failed", ErrorCode: "FUTURE"}} {
		if _, e := Error(a, 0); e == nil {
			t.Fatal("invalid error accepted")
		}
	}
	if _, e := Error(domain.Answer{Status: "failed", ErrorCode: "MODEL_ERROR"}, -1); e == nil {
		t.Fatal("negative limit accepted")
	}
}

func TestUnitGeneratingSnapshotRetainsReconnectState(t *testing.T) {
	a := domain.Answer{Status: "generating", Chips: []json.RawMessage{json.RawMessage(`{"label":"analytics","screen":"analytics","params":{"period":"month","date":"2026-09","transfer_mode":"with"}}`)}, Source: &domain.Source{CalculatedAt: time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC), TransferMode: "with"}}
	snapshot, err := AnswerSnapshot(a)
	if err != nil || len(snapshot.Chips) != 1 || snapshot.Source == nil {
		t.Fatalf("snapshot %v, %v", snapshot, err)
	}
	message, err := Message(domain.Message{CreatedAt: a.Source.CalculatedAt, Answer: &a})
	if err != nil || len(message.Chips) != 0 || message.Source != nil {
		t.Fatalf("history %v, %v", message, err)
	}
}

func TestUnitRowsStringValue(t *testing.T) {
	for _, row := range []string{`{"label":"text","value_kind":"text","value":"available"}`, `{"label":"text","text":"available"}`} {
		blocks, err := Blocks([]domain.Block{{Type: "rows", Version: 1, Body: json.RawMessage(`{"alt_text":"x","data":{"rows":[` + row + `]}}`)}})
		if err != nil || blocks[0].GetRows().Rows[0].GetText() != "available" {
			t.Fatalf("%v %v", blocks, err)
		}
	}
	for _, row := range []string{`{"label":"x","value_kind":"text","value":1}`, `{"label":"x","value_kind":"text","value":null}`, `{"label":"x","value_kind":"amount","value":"123"}`, `{"label":"x","value_kind":"count","value":null}`, `{"label":"x","value_kind":"percent","value":"0.5"}`} {
		_, err := Blocks([]domain.Block{{Type: "rows", Version: 1, Body: json.RawMessage(`{"alt_text":"x","data":{"rows":[` + row + `]}}`)}})
		if err == nil {
			t.Fatalf("accepted %s", row)
		}
	}
}

func TestUnitUnknownChartShapeUsesAltBeforeTypedDecode(t *testing.T) {
	for _, raw := range []string{
		`{"alt_text":"equivalent","data":{"kind":"pie","value_kind":"amount","points":["new-shape"]}}`,
		`{"alt_text":"equivalent","data":{"kind":"bar","value_kind":"future","points":[{"value":"new-unit","range":"new-range"}]}}`,
		`{"alt_text":"equivalent","data":{"kind":"bar","value_kind":"future","rows":"different"}}`,
	} {
		blocks, err := Blocks([]domain.Block{{Type: "chart", Version: 1, Body: json.RawMessage(raw)}})
		if err != nil || len(blocks) != 1 || blocks[0].GetText().Text != "equivalent" {
			t.Fatalf("%v %v", blocks, err)
		}
	}
	for _, raw := range []string{
		`{"alt_text":"equivalent","data":{"kind":"bar","value_kind":"amount","points":["corrupt"]}}`,
		`{"alt_text":"equivalent","data":"corrupt"}`,
	} {
		if _, err := Blocks([]domain.Block{{Type: "chart", Version: 1, Body: json.RawMessage(raw)}}); err == nil {
			t.Fatal("corrupt known chart accepted")
		}
	}
}
