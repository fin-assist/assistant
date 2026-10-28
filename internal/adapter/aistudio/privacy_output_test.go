package aistudio

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestUnitPrivacyOutputRestoresSplitTokensAndConfirmedRaw(t *testing.T) {
	f := privacyFixture()
	token, _ := f.text("+7 (999) 123-45-67")
	unknown := "[phone_000000000000000000000000]"
	original := "До " + token + " после " + unknown
	want := "До +7 (999) 123-45-67 после " + unknown
	for cut := 1; cut < len(token); cut++ {
		var emitted strings.Builder
		r := newOutputRestorer(f.replacements, 4096, func(d Delta) error { emitted.WriteString(d.Text); return nil })
		for _, text := range []string{"До " + token[:cut], token[cut:] + " после " + unknown} {
			if err := r.delta(Delta{Text: text}); err != nil {
				t.Fatal(err)
			}
			for _, pending := range r.pending {
				if len(pending.delta.Text) >= len(token) {
					t.Fatal("carry is not a proper token prefix")
				}
			}
		}
		if err := r.done(Delta{}); err != nil {
			t.Fatal(err)
		}
		if err := r.finish(); err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(map[string]any{"id": "m", "type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": original}}})
		result, err := r.result(Result{Output: []Item{{ID: "m", Type: "message", Raw: raw}}})
		if err != nil || emitted.String() != want || !strings.Contains(string(result.Output[0].Raw), "+7 (999) 123-45-67") || strings.Contains(string(result.Output[0].Raw), token) {
			t.Fatal("stream/final message restoration disagrees", err)
		}
	}
}
func TestUnitPrivacyOutputKeepsOrderAndFlushesUnknownPrefix(t *testing.T) {
	f := privacyFixture()
	token, _ := f.text("test@example.test")
	var emitted strings.Builder
	r := newOutputRestorer(f.replacements, 4096, func(d Delta) error { emitted.WriteString(d.Text); return nil })
	if err := r.delta(Delta{Text: token[:4]}); err != nil {
		t.Fatal(err)
	}
	if err := r.delta(Delta{OutputIndex: 1, Text: " next"}); err != nil {
		t.Fatal(err)
	}
	if emitted.Len() != 0 {
		t.Fatal("later part overtook pending token")
	}
	if err := r.delta(Delta{Text: token[4:]}); err != nil {
		t.Fatal(err)
	}
	if err := r.done(Delta{}); err != nil {
		t.Fatal(err)
	}
	if err := r.done(Delta{OutputIndex: 1}); err != nil {
		t.Fatal(err)
	}
	if emitted.String() != "test@example.test next" {
		t.Fatal("part order changed", emitted.String())
	}
	r = newOutputRestorer(f.replacements, 4096, func(d Delta) error { emitted.WriteString(d.Text); return nil })
	emitted.Reset()
	if err := r.delta(Delta{Text: "ordinary [ema"}); err != nil {
		t.Fatal(err)
	}
	if err := r.finish(); !errors.Is(err, errPrivacy) {
		t.Fatal("unfinished parser part was silently flushed")
	}
	if err := r.done(Delta{}); err != nil {
		t.Fatal(err)
	}
	if err := r.finish(); err != nil || emitted.String() != "ordinary [ema" {
		t.Fatal("unknown incomplete token changed", err)
	}
}
func TestUnitPrivacyOutputBoundsExpansionAndPropagatesSinkErrors(t *testing.T) {
	token := "[email_111111111111111111111111]"
	replacements := []privacyReplacement{{token: token, value: strings.Repeat("x", 128)}}
	emits := 0
	r := newOutputRestorer(replacements, 64, func(Delta) error { emits++; return nil })
	if err := r.delta(Delta{Text: token}); !errors.Is(err, errPrivacy) || emits != 0 {
		t.Fatal("unbounded restored expansion", err)
	}
	r = newOutputRestorer(replacements, 4, func(Delta) error { emits++; return nil })
	if err := r.delta(Delta{Text: "[email_"}); !errors.Is(err, errPrivacy) {
		t.Fatal("carry exceeded output bound", err)
	}
	r = newOutputRestorer(replacements, 8, func(Delta) error { emits++; return nil })
	if err := r.delta(Delta{Text: "[email_"}); err != nil {
		t.Fatal(err)
	}
	if err := r.delta(Delta{Text: "111"}); !errors.Is(err, errPrivacy) {
		t.Fatal("growing carry exceeded output bound", err)
	}
	sentinel := errors.New("synthetic sink error")
	for _, masked := range []bool{false, true} {
		entries := replacements
		if !masked {
			entries = nil
		}
		r = newOutputRestorer(entries, 4096, func(Delta) error { return sentinel })
		if err := r.delta(Delta{Text: "ordinary"}); !errors.Is(err, sentinel) {
			t.Fatal("sink error changed", err)
		}
	}
	r = newOutputRestorer(replacements, 4096, func(Delta) error { return sentinel })
	if err := r.delta(Delta{Text: "[ema"}); err != nil {
		t.Fatal(err)
	}
	if err := r.done(Delta{}); !errors.Is(err, sentinel) {
		t.Fatal("flush sink error changed", err)
	}
}

func TestUnitPrivacyOutputLabelsBareValuesWithoutChangingDigits(t *testing.T) {
	for _, tc := range []struct{ label, value string }{{"карта ", "4111111111111111"}, {"карта ", "4111111111111112"}, {"телефон ", "79991234567"}, {"телефон ", "89991234567"}, {"счёт ", "40817810099910004312"}} {
		f := privacyFixture()
		token, err := f.text(tc.label + tc.value)
		if err != nil {
			t.Fatal(err)
		}
		token = strings.TrimPrefix(token, tc.label)
		r := newOutputRestorer(f.replacements, 4096, func(Delta) error { return nil })
		display, err := r.restore(token, 4096)
		if err != nil || display != tc.label+tc.value {
			t.Fatal("display changed raw requisite", display, err)
		}
		fresh := privacyFixture()
		fresh.scope = "next-answer"
		masked, err := fresh.text(display)
		if err != nil || strings.Contains(masked, tc.value) {
			t.Fatal("tool-only restored value escaped next history", masked, err)
		}
		restored := restoreSearchArguments("find_operations", `{"filter":{"q":"`+strings.TrimPrefix(masked, tc.label)+`"}}`, fresh.replacements)
		if !strings.Contains(restored, tc.value) {
			t.Fatal("follow-up search lost exact raw digits", restored)
		}
	}
}

func TestUnitPrivacyOutputPreservesNaturalLabelsAcrossDeltaBoundaries(t *testing.T) {
	for _, tc := range []struct{ source, prefix, value string }{
		{"карта ", "Списания по карте ", "4111111111111111"},
		{"телефон ", "Переводы на телефон ", "79991234567"},
		{"телефон ", "Переводы по телефону ", "79991234567"},
		{"счёт ", "Движение по счёту ", "40817810099910004312"},
	} {
		f := privacyFixture()
		masked, _ := f.text(tc.source + tc.value)
		token := strings.TrimPrefix(masked, tc.source)
		for cut := 1; cut < len(token); cut++ {
			var emitted strings.Builder
			r := newOutputRestorer(f.replacements, 4096, func(delta Delta) error { emitted.WriteString(delta.Text); return nil })
			runes := []rune(tc.prefix)
			middle := len(runes) / 2
			for _, part := range []string{string(runes[:middle]), string(runes[middle:]) + token[:cut], token[cut:]} {
				if err := r.delta(Delta{Text: part}); err != nil {
					t.Fatal(err)
				}
			}
			if err := r.done(Delta{}); err != nil {
				t.Fatal(err)
			}
			restored, err := r.restore(tc.prefix+token, 4096)
			want := tc.prefix + tc.value
			if err != nil || restored != want || emitted.String() != want {
				t.Fatal("label duplicated or final Raw disagrees", emitted.String(), restored, err)
			}
			for _, prefix := range r.context {
				if len(prefix) > 64 {
					t.Fatal("label context exceeded its bound")
				}
			}
		}
	}
}
