package domain

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestOffTopicAdmissionUsesStrictRollingWindowAndThirdExpiry(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	policy := OffTopicPolicy{Max: 5, Window: 7 * 24 * time.Hour}
	var completed []time.Time
	for i := range 7 {
		completed = append(completed, now.Add(time.Duration(i-6)*24*time.Hour))
	}
	before := append([]time.Time(nil), completed...)
	want := now.Add(3 * 24 * time.Hour)
	for _, at := range []time.Time{now, want.Add(-time.Nanosecond)} {
		got, err := policy.RetryAt(at, completed)
		if err != nil || !got.Equal(want) {
			t.Fatalf("retryAt(%s)=%s %v want third expiry%s", at, got, err, want)
		}
	}
	if !reflect.DeepEqual(before, completed) {
		t.Fatal("policy reordered caller's saved facts")
	}
	for _, at := range []time.Time{want, want.Add(time.Nanosecond)} {
		got, err := policy.RetryAt(at, completed)
		if err != nil || !got.IsZero() {
			t.Fatalf("fact still counted at exact expiry: %s %v", got, err)
		}
	}
	query, err := policy.Query(now)
	if err != nil || !query.After.Equal(now.Add(-policy.Window)) || query.Latest != 5 {
		t.Fatalf("wrong fact request: %+v %v", query, err)
	}
	// Storage need only return the five newest facts, even after inflight overflow.
	got, err := policy.RetryAt(now, []time.Time{completed[6], completed[5], completed[4], completed[3], completed[2]})
	if err != nil || !got.Equal(want) {
		t.Fatalf("bounded latest facts changed decision: %s %v", got, err)
	}
}

func TestOffTopicPolicyConfigurationClockRollbackAndEmptyFacts(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	policy := OffTopicPolicy{Max: 2, Window: 2 * time.Hour}
	for _, completed := range [][]time.Time{nil, {now}, {now.Add(-2 * time.Hour), now}} {
		if got, err := policy.RetryAt(now, completed); err != nil || !got.IsZero() {
			t.Fatalf("allowed facts blocked: %v %s %v", completed, got, err)
		}
	}
	// A clock rollback cannot forgive later committed outcomes.
	future := now.Add(time.Hour)
	got, err := policy.RetryAt(now, []time.Time{future, future})
	if err != nil || !got.Equal(future.Add(policy.Window)) {
		t.Fatalf("clock rollback forgave outcomes: %s %v", got, err)
	}
	for _, bad := range []OffTopicPolicy{{}, {Max: -1, Window: time.Hour}, {Max: 32768, Window: time.Hour}, {Max: 5, Window: time.Nanosecond}} {
		if _, err := bad.Query(now); !errors.Is(err, ErrInvalidOffTopicPolicy) {
			t.Fatalf("invalid policy accepted: %+v %v", bad, err)
		}
		if _, err := bad.RetryAt(now, nil); !errors.Is(err, ErrInvalidOffTopicPolicy) {
			t.Fatalf("invalid policy admitted: %+v %v", bad, err)
		}
	}
	if _, err := policy.Query(time.Time{}); !errors.Is(err, ErrInvalidOffTopicPolicy) {
		t.Fatal("missing clock accepted")
	}
}

func TestOffTopicTerminalEffectsAndMoscowDiagnosticDay(t *testing.T) {
	before := time.Date(2026, 10, 23, 20, 59, 59, 999999000, time.UTC)
	for _, tc := range []struct {
		code    string
		charged bool
		action  OffTopicLedgerAction
	}{{"", true, ResetOffTopic}, {"", false, KeepOffTopic}, {"MODEL_ERROR", false, KeepOffTopic}, {"CANNOT_ANSWER", false, KeepOffTopic}, {"OFF_TOPIC", false, RecordOffTopic}} {
		effect := PlanOffTopic(tc.code, tc.charged, before)
		if effect.Action != tc.action {
			t.Fatalf("wrong terminal policy %+v: %+v", tc, effect)
		}
	}
	a, b := PlanOffTopic("OFF_TOPIC", false, before), PlanOffTopic("OFF_TOPIC", false, before.Add(time.Microsecond))
	if a.DayStart.Format(time.RFC3339) != "2026-10-23T00:00:00+03:00" || !a.DayEnd.Equal(b.DayStart) || b.DayEnd.Sub(b.DayStart) != 24*time.Hour {
		t.Fatalf("diagnostic day follows profile zone or crosses midnight: %+v %+v", a, b)
	}
}
