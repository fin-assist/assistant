package app

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/fin-assist/assistant/internal/adapter/client"
	"github.com/fin-assist/assistant/internal/generation"
	"github.com/fin-assist/assistant/internal/tooldata"
)

func TestGenerationMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, err := newGenerationMetrics(reg, metricRegistry(t))
	if err != nil {
		t.Fatal(err)
	}
	m.accepted.Inc()
	m.Step(generation.Usage{InputTokens: 10, OutputTokens: 4, TotalTokens: 14, ToolTokens: 2})
	m.Step(generation.Usage{InputTokens: -1, OutputTokens: -1, TotalTokens: -1, ToolTokens: -1})
	m.Function("aggregate_operations")
	m.Function("present_result")
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
		"finassist_assistant_function_calls_total/function=present_result":       1,
		"finassist_assistant_function_calls_total/function=unknown":              1,
		"finassist_assistant_refunds_total":                                      2,
	} {
		if values[key] != want {
			t.Errorf("%s=%v, want %v", key, values[key], want)
		}
	}
	if _, err := newGenerationMetrics(reg, metricRegistry(t)); err == nil {
		t.Fatal("duplicate registration accepted")
	}
}

func TestOffTopicDailyMetricHasNoIdentityAndIgnoresInvalidCounts(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, err := newGenerationMetrics(reg, metricRegistry(t))
	if err != nil {
		t.Fatal(err)
	}
	m.Function("off_topic")
	m.Finished("OFF_TOPIC", false, time.Second)
	m.OffTopicDailyCount(1)
	m.OffTopicDailyCount(3)
	m.OffTopicDailyCount(0)
	m.OffTopicDailyCount(-1)
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, family := range families {
		if family.GetName() != "finassist_assistant_off_topic_daily_count" {
			continue
		}
		found = true
		if len(family.Metric) != 1 {
			t.Fatal("unexpected identity cardinality")
		}
		metric := family.Metric[0]
		if len(metric.Label) != 0 || metric.Histogram.GetSampleCount() != 2 || metric.Histogram.GetSampleSum() != 4 {
			t.Fatalf("invalid count distribution: %+v", metric)
		}
	}
	if !found {
		t.Fatal("missing daily count histogram")
	}
}

func metricRegistry(t *testing.T) *generation.ToolRegistry {
	t.Helper()
	data, err := client.NewFinancialData(nil, nil, client.FinancialLimits{ResultBytes: 1, CollectionItems: 1})
	if err != nil {
		t.Fatal(err)
	}
	tools, err := generation.NewToolRegistry(generation.BuiltinTools(data)...)
	if err != nil {
		t.Fatal(err)
	}
	return tools
}

func TestGenerationMetricAllowlistComesFromRegistry(t *testing.T) {
	tool := generation.NewReadTool(generation.ToolDefinition{Name: "extension_read", Description: "A new typed reader", Parameters: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{}}`)}, func(json.RawMessage) (struct{}, error) { return struct{}{}, nil }, func(context.Context, tooldata.Subject, struct{}) (tooldata.Result, error) {
		return tooldata.Result{}, nil
	}, false)
	tools, err := generation.NewToolRegistry(tool)
	if err != nil {
		t.Fatal(err)
	}
	reg := prometheus.NewRegistry()
	metrics, err := newGenerationMetrics(reg, tools)
	if err != nil {
		t.Fatal(err)
	}
	metrics.Function("extension_read")
	metrics.Function("private-dynamic-input")
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]float64{}
	for _, family := range families {
		if family.GetName() == "finassist_assistant_function_calls_total" {
			for _, metric := range family.Metric {
				found[metric.Label[0].GetValue()] = metric.Counter.GetValue()
			}
		}
	}
	if len(found) != 2 || found["extension_read"] != 1 || found["unknown"] != 1 {
		t.Fatalf("allowlist diverged from tool registry: %+v", found)
	}
	if _, err := newGenerationMetrics(prometheus.NewRegistry(), nil); err == nil {
		t.Fatal("metrics accepted missing registry")
	}
}
