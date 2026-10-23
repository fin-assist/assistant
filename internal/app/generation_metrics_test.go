package app

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/fin-assist/assistant/internal/generation"
)

func TestGenerationMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, err := newGenerationMetrics(reg)
	if err != nil {
		t.Fatal(err)
	}
	m.accepted.Inc()
	m.Step(generation.Usage{InputTokens: 10, OutputTokens: 4, TotalTokens: 14, ToolTokens: 2})
	m.Step(generation.Usage{InputTokens: -1, OutputTokens: -1, TotalTokens: -1, ToolTokens: -1})
	m.Function("aggregate_operations")
	m.Function("private-user-dialogue")
	m.Searches(3)
	m.Searches(-1)
	m.Finished("complete", true, time.Second)
	m.Finished("MODEL_ERROR", false, 2*time.Second)
	m.Finished("private-model-error", false, -time.Second)
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]float64{}
	for _, family := range families {
		for _, metric := range family.Metric {
			key := family.GetName()
			for _, label := range metric.Label {
				key += "/" + label.GetName() + "=" + label.GetValue()
				if label.GetValue() == "private-user-dialogue" || label.GetValue() == "private-model-error" {
					t.Fatal("unbounded metric label")
				}
			}
			if metric.Counter != nil {
				values[key] = metric.Counter.GetValue()
			}
		}
	}
	for key, want := range map[string]float64{
		"finassist_assistant_questions_accepted_total":                           1,
		"finassist_assistant_search_calls_total":                                 3,
		"finassist_assistant_model_tokens_total/kind=input":                      10,
		"finassist_assistant_model_tokens_total/kind=output":                     4,
		"finassist_assistant_model_tokens_total/kind=total":                      14,
		"finassist_assistant_model_tokens_total/kind=tool":                       2,
		"finassist_assistant_function_calls_total/function=aggregate_operations": 1,
		"finassist_assistant_function_calls_total/function=unknown":              1,
		"finassist_assistant_refunds_total":                                      2,
	} {
		if values[key] != want {
			t.Errorf("%s=%v, want %v", key, values[key], want)
		}
	}
	if _, err := newGenerationMetrics(reg); err == nil {
		t.Fatal("duplicate registration accepted")
	}
}
