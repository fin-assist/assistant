package aistudio

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func privacyFixture() *outboundRedactor {
	return &outboundRedactor{key: []byte("synthetic-test-only-key"), scope: "synthetic-answer"}
}
func TestUnitPrivacyRecognizesOnlySupportedRequisites(t *testing.T) {
	for _, tc := range []struct{ text, kind string }{
		{"Email: test.person+tag@example.test", "email"}, {"почта: пример@пример.рф", "email"},
		{"+7 (999) 123-45-67", "phone"}, {"8-999-123-45-67", "phone"}, {"телефон: 79991234567", "phone"},
		{"phone: 89991234567", "phone"}, {"+7\u202f999\u00a0123-45-67", "phone"},
		{"карта 4111111111111111", "card"}, {"по карте 4111111111111111", "card"}, {"на карту 4111111111111111", "card"}, {"картой: 4111111111111111", "card"}, {"картою 4111111111111111", "card"}, {"4111 1111 1111 1111", "card"}, {"3782-822463-10005", "card"},
		{"счёт: 40817810099910004312", "account"}, {"account 40817810099910004312", "account"}, {"40817.810.0.9991.0004312", "account"},
	} {
		t.Run(tc.text, func(t *testing.T) {
			f := privacyFixture()
			got, err := f.text(tc.text)
			if err != nil || !strings.Contains(got, "["+tc.kind+"_") || len(f.replacements) != 1 {
				t.Fatalf("supported requisite not replaced: kind=%s err=%v", tc.kind, err)
			}
			if f.replacements[0].value == "" || strings.Contains(got, f.replacements[0].value) {
				t.Fatal("original value escaped filter")
			}
		})
	}
	for _, text := range []string{
		"9223372036854775807", "9007199254740993", "40817810099910004312", "79991234567", "123456789012345678901",
		"4111111111111111", "4111 1111 1111 1112", "0000 0000 0000 0000", "100.20 RUB 2026-10-28 12:30:00",
		"Сумма 1 234 567 890 рублей, 12.50 USD; остаток 89991234567", "аптека, лекарства, здоровье; Т-Банк, Магнит, Иван Петров",
		"Счёт ··4821, текущий; 2026-10-28", "Номер операции 12345678901234567890",
	} {
		got, err := privacyFixture().text(text)
		if err != nil || got != text {
			t.Fatalf("financial/free text changed: %q -> %q (%v)", text, got, err)
		}
	}
}
func TestUnitPrivacyAliasesPreserveLinksAndIsolateGenerations(t *testing.T) {
	f := privacyFixture()
	first, _ := f.text("+7 (999) 123-45-67")
	repeated, _ := f.text("8-999-123-45-67")
	other, _ := f.text("+7 (999) 123-45-68")
	different := privacyFixture()
	different.scope = "other-answer"
	isolated, _ := different.text("+7 (999) 123-45-67")
	if first != repeated || first == other || first == isolated {
		t.Fatal("aliases merged different requisites or crossed generation boundary")
	}
}
func TestUnitPrivacyFiltersNestedCopiesAndPreservesExactNumbers(t *testing.T) {
	raw := json.RawMessage(`{"type":"function_call_output","call_id":"unique-item","output":"{\"description\":\"test@example.test +7 (999) 123-45-67 карта 4111111111111111\",\"amount_minor\":\"9223372036854775807\",\"exact\":9007199254740993,\"at\":\"2026-10-28\"}"}`)
	before := string(raw)
	request := httpTestRequest()
	request.PrivacyScope = "never-outbound"
	request.Instructions = "Contact test@example.test"
	request.Input = append(request.Input, raw, json.RawMessage(`{"type":"reasoning","encrypted_content":"opaque-original-requisites"}`))
	request.Tools = []json.RawMessage{json.RawMessage(`{"type":"web_search"}`)}
	filtered, err := privacyFixture().request(request)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != before || len(request.Input) != 3 || len(filtered.Input) != 2 {
		t.Fatal("filter changed original request or retained reasoning")
	}
	body, _ := json.Marshal(filtered)
	for _, forbidden := range []string{"test@example.test", "999", "4111111111111111", "opaque-original", "never-outbound"} {
		if strings.Contains(string(body), forbidden) {
			t.Fatalf("filtered request still contains %s", forbidden)
		}
	}
	for _, preserved := range []string{"9223372036854775807", "9007199254740993", "2026-10-28", "unique-item", "web_search"} {
		if !strings.Contains(string(body), preserved) {
			t.Fatalf("financial value/protocol field lost: %s", preserved)
		}
	}
}
func TestUnitPrivacyMalformedInputFailsBeforeAnyEffect(t *testing.T) {
	for _, raw := range []string{
		`{"type":"function_call_output","output":"{\"x\":1,\"x\":2}"}`,
		`{"type":"function_call_output","output":"{broken"}`,
		`{"type":"function_call","arguments":"{broken"}`,
		`{"content":"test@example.test","content":"other"}`,
		`{"content":"` + string([]byte{255}) + `"}`,
		`{"content":` + strings.Repeat("[", 65) + `"private"` + strings.Repeat("]", 65) + `}`,
		`{"type":"reasoning","encrypted_content":"opaque"}`,
	} {
		t.Run(raw[:min(30, len(raw))], func(t *testing.T) {
			opts := httpTestOptions()
			keys, posts, guards, emits := 0, 0, 0, 0
			opts.Key = httpTestKey(func(_ context.Context) (string, error) { keys++; return "key", nil })
			client, err := New(opts)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			client.http.Transport = httpTestRoundTrip(func(*http.Request) (*http.Response, error) { posts++; return nil, errors.New("unexpected") })
			req := httpTestRequest()
			req.Input = []json.RawMessage{json.RawMessage(raw)}
			_, err = client.StreamGuarded(httpTestContext(t), req, func(Delta) error { emits++; return nil }, func([]byte) error { guards++; return nil })
			if err == nil || keys+posts+guards+emits != 0 {
				t.Fatalf("unsafe filter failure: err=%v effects=%d", err, keys+posts+guards+emits)
			}
			if strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "example.test") {
				t.Fatal("filter error contains original input")
			}
		})
	}
}
func TestUnitPrivacyRestoresOnlyDeclaredLocalSearchField(t *testing.T) {
	f := privacyFixture()
	token, _ := f.text("+7 (999) 123-45-67")
	args, _ := json.Marshal(map[string]any{"filter": map[string]any{"q": token, "selection": token}, "other": token})
	for _, name := range []string{"find_operations", "aggregate_operations", "get_analytics", "web_search", "present_result"} {
		got, err := restoreSearchArguments(name, string(args), f.replacements)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]any
		_ = json.Unmarshal([]byte(got), &fields)
		query := fields["filter"].(map[string]any)["q"]
		want := token
		if name == "find_operations" || name == "aggregate_operations" {
			want = "+7 (999) 123-45-67"
		}
		if query != want || fields["filter"].(map[string]any)["selection"] != token || fields["other"] != token {
			t.Fatal("restoration escaped declared local q")
		}
	}
	for _, args := range []string{`{}`, `{"filter":{}}`, `{"filter":null}`, `{"filter":{"q":7}}`, `{"filter":7}`, `{broken`} {
		got, err := restoreSearchArguments("find_operations", args, f.replacements)
		if args == `{}` || args == `{"filter":{}}` || args == `{"filter":null}` {
			if err != nil || got != args {
				t.Fatal(got, err)
			}
		} else if err == nil {
			t.Fatal("invalid local search arguments accepted")
		}
	}
}
func TestUnitPrivacyBudgetSeesFilteredWireOnly(t *testing.T) {
	client, err := New(httpTestOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var sent, reserved []byte
	client.http.Transport = httpTestRoundTrip(func(req *http.Request) (*http.Response, error) {
		sent, _ = io.ReadAll(req.Body)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(httpTestSSE("done")))}, nil
	})
	req := httpTestRequest()
	req.Input = []json.RawMessage{json.RawMessage(`{"role":"user","content":"test@example.test"}`)}
	_, err = client.StreamGuarded(httpTestContext(t), req, func(Delta) error { return nil }, func(body []byte) error { reserved = append([]byte{}, body...); return nil })
	if err != nil || !reflect.DeepEqual(reserved, sent) || strings.Contains(string(sent), "test@example.test") {
		t.Fatal("wire/reservation filtering mismatch", err)
	}
}

func TestUnitPrivacyCardLabelDoesNotConsumeAdjacentAmount(t *testing.T) {
	got, err := privacyFixture().text("карта 4111111111111111 100 рублей")
	if err != nil || !strings.HasSuffix(got, " 100 рублей") || strings.Contains(got, "411111") {
		t.Fatal(got, err)
	}
}

func TestUnitPrivacyUnchangedEmbeddedJSONKeepsAcceptedWireForm(t *testing.T) {
	raw := json.RawMessage(`{"type":"function_call_output","output":"{\"z\": 9007199254740993, \"a\": \"ordinary data\"}"}`)
	filtered, err := privacyFixture().json(raw, 0)
	if err != nil || string(filtered) != string(raw) {
		t.Fatal("unchanged embedded JSON was rewritten", err)
	}
	if _, err := privacyFixture().text(string([]byte{255})); err == nil {
		t.Fatal("invalid text encoding accepted")
	}
}

func TestUnitPrivacyRejectsAliasedObjectKeyCollision(t *testing.T) {
	raw := json.RawMessage(`{"+7 (999) 123-45-67":"one","8-999-123-45-67":"two"}`)
	if _, err := privacyFixture().json(raw, 0); !errors.Is(err, errPrivacy) {
		t.Fatal("alias key collision silently merged values", err)
	}
	raw = json.RawMessage(`{"metadata":{"encrypted_content":"opaque-original","public":"ordinary"}}`)
	filtered, err := privacyFixture().json(raw, 0)
	if err != nil || strings.Contains(string(filtered), "opaque-original") || !strings.Contains(string(filtered), "ordinary") {
		t.Fatal("nested opaque data was forwarded", err)
	}
}
