package app

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/fin-assist/assistant/internal/generation"
)

type generationMetrics struct {
	accepted  prometheus.Counter
	finished  *prometheus.CounterVec
	duration  *prometheus.HistogramVec
	functions *prometheus.CounterVec
	searches  prometheus.Counter
	tokens    *prometheus.CounterVec
	refunded  prometheus.Counter
}

func newGenerationMetrics(reg prometheus.Registerer) (*generationMetrics, error) {
	m := &generationMetrics{
		accepted:  prometheus.NewCounter(prometheus.CounterOpts{Name: "finassist_assistant_questions_accepted_total", Help: "Committed questions and retries started by this process."}),
		finished:  prometheus.NewCounterVec(prometheus.CounterOpts{Name: "finassist_assistant_generations_total", Help: "Committed terminal answers by outcome and charged flag."}, []string{"outcome", "charged"}),
		duration:  prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "finassist_assistant_generation_seconds", Help: "Duration of terminal answer generation.", Buckets: []float64{.1, .5, 1, 2, 5, 10, 30, 60, 120, 180}}, []string{"outcome"}),
		functions: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "finassist_assistant_function_calls_total", Help: "Own function calls by bounded registry name."}, []string{"function"}),
		searches:  prometheus.NewCounter(prometheus.CounterOpts{Name: "finassist_assistant_search_calls_total", Help: "Observed completed provider search calls."}),
		tokens:    prometheus.NewCounterVec(prometheus.CounterOpts{Name: "finassist_assistant_model_tokens_total", Help: "Provider-reported usage; counters are separate, not additive billing totals."}, []string{"kind"}),
		refunded:  prometheus.NewCounter(prometheus.CounterOpts{Name: "finassist_assistant_refunds_total", Help: "Committed terminal answers whose attempt was refunded."}),
	}
	for _, c := range []prometheus.Collector{m.accepted, m.finished, m.duration, m.functions, m.searches, m.tokens, m.refunded} {
		if err := reg.Register(c); err != nil {
			return nil, err
		}
	}
	return m, nil
}

func (m *generationMetrics) Step(u generation.Usage) {
	for kind, value := range map[string]int64{"input": u.InputTokens, "output": u.OutputTokens, "total": u.TotalTokens, "tool": u.ToolTokens} {
		if value >= 0 {
			m.tokens.WithLabelValues(kind).Add(float64(value))
		}
	}
}
func (m *generationMetrics) Function(name string) {
	switch name {
	case "get_data_overview", "aggregate_operations", "find_operations", "get_analytics", "list_periods", "present_result", "cannot_answer":
	default:
		name = "unknown"
	}
	m.functions.WithLabelValues(name).Inc()
}
func (m *generationMetrics) Searches(n int) {
	if n >= 0 {
		m.searches.Add(float64(n))
	}
}
func (m *generationMetrics) Finished(code string, charged bool, duration time.Duration) {
	switch code {
	case "complete", "MODEL_ERROR", "CANNOT_ANSWER", "OFF_TOPIC":
	default:
		code = "unknown"
	}
	flag := "false"
	if charged {
		flag = "true"
	}
	m.finished.WithLabelValues(code, flag).Inc()
	m.duration.WithLabelValues(code).Observe(max(0, duration.Seconds()))
	if !charged {
		m.refunded.Inc()
	}
}
