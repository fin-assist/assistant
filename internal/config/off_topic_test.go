package config

import (
	"strings"
	"testing"
	"time"

	grpcdialogue "github.com/fin-assist/assistant/internal/adapter/grpc/dialogue"
	"github.com/fin-assist/assistant/internal/dialogue"
)

func TestOffTopicDefaultsAndContractText(t *testing.T) {
	get, list := env(base())
	cfg, err := load(get, noFiles, list)
	if err != nil {
		t.Fatal(err)
	}
	defaults := DefaultOffTopicConfig()
	if cfg.OffTopic != defaults || defaults.Max != 5 || defaults.Window != 168*time.Hour || defaults.Text == "" {
		t.Fatalf("off-topic defaults: %+v", cfg.OffTopic)
	}
	legacy, err := grpcdialogue.Error(dialogue.Answer{Status: "failed", ErrorCode: "OFF_TOPIC"}, 5)
	if err != nil || legacy.GetText() != defaults.Text {
		t.Fatalf("default text drifted from legacy contract mapping: %v %v", legacy, err)
	}
}
func TestOffTopicOverridesAndPrecisionBoundaries(t *testing.T) {
	vars := base()
	vars["ASSISTANT_OFF_TOPIC_MAX"] = "32767"
	vars["ASSISTANT_OFF_TOPIC_WINDOW"] = "1ms"
	vars["ASSISTANT_OFF_TOPIC_TEXT"] = " \tСвоя серверная подсказка\n"
	get, list := env(vars)
	cfg, err := load(get, noFiles, list)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OffTopic.Max != 32767 || cfg.OffTopic.Window != time.Millisecond || cfg.OffTopic.Text != "Своя серверная подсказка" {
		t.Fatalf("override=%+v", cfg.OffTopic)
	}
	vars["ASSISTANT_OFF_TOPIC_MAX"] = "1"
	vars["ASSISTANT_OFF_TOPIC_TEXT"] = strings.Repeat("x", 4096)
	get, list = env(vars)
	cfg, err = load(get, noFiles, list)
	if err != nil || cfg.OffTopic.Max != 1 || len(cfg.OffTopic.Text) != 4096 {
		t.Fatalf("valid lower/count and upper/text boundaries: %v", err)
	}
}
func TestOffTopicInvalidSettingsFailWithoutEchoingText(t *testing.T) {
	for _, tc := range []struct{ name, value string }{
		{"ASSISTANT_OFF_TOPIC_MAX", "0"}, {"ASSISTANT_OFF_TOPIC_MAX", "-1"}, {"ASSISTANT_OFF_TOPIC_MAX", "32768"}, {"ASSISTANT_OFF_TOPIC_MAX", "1.5"},
		{"ASSISTANT_OFF_TOPIC_WINDOW", "0"}, {"ASSISTANT_OFF_TOPIC_WINDOW", "-1h"}, {"ASSISTANT_OFF_TOPIC_WINDOW", "999999ns"}, {"ASSISTANT_OFF_TOPIC_WINDOW", "1ms1ns"}, {"ASSISTANT_OFF_TOPIC_WINDOW", "bad-duration"}, {"ASSISTANT_OFF_TOPIC_WINDOW", "999999999999999h"},
		{"ASSISTANT_OFF_TOPIC_TEXT", "  \t\n"}, {"ASSISTANT_OFF_TOPIC_TEXT", string([]byte{0xff})}, {"ASSISTANT_OFF_TOPIC_TEXT", strings.Repeat("private-text", 400)},
	} {
		t.Run(tc.name+"-"+tc.value[:min(len(tc.value), 12)], func(t *testing.T) {
			vars := base()
			vars[tc.name] = tc.value
			get, list := env(vars)
			_, err := load(get, noFiles, list)
			if err == nil || !strings.Contains(err.Error(), tc.name) {
				t.Fatalf("invalid %s accepted: %v", tc.name, err)
			}
			if strings.Contains(err.Error(), "private-text") {
				t.Fatal("configured display text leaked into validation error")
			}
		})
	}
	cfg := Config{OffTopic: DefaultOffTopicConfig()}
	cfg.OffTopic.Text = ""
	if len(cfg.validateOffTopic()) == 0 {
		t.Fatal("direct assembly accepted empty display text")
	}
}
