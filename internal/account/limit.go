// Package account owns the assistant's per-user settings, limit and deletion use cases.
package account

import "time"

// State is the persisted daily counter. LimitDay is a calendar date, not an instant.
type State struct {
	LimitDay time.Time
	Used     int
}

// Limit is the daily allowance and its next Moscow reset.
type Limit struct {
	Remaining int
	DailyMax  int
	ResetsAt  time.Time
}

// CalculateLimit never resets a counter backwards when instances disagree near midnight.
func CalculateLimit(now time.Time, state State, dailyMax int) Limit {
	moscow := time.FixedZone("Europe/Moscow", 3*60*60)
	n := now.In(moscow)
	day := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, moscow)
	remaining := dailyMax
	if !state.LimitDay.IsZero() {
		stored := time.Date(state.LimitDay.Year(), state.LimitDay.Month(), state.LimitDay.Day(), 0, 0, 0, 0, moscow)
		if !stored.Before(day) {
			day = stored
			remaining = max(0, dailyMax-state.Used)
		}
	}
	return Limit{Remaining: remaining, DailyMax: dailyMax, ResetsAt: day.AddDate(0, 0, 1)}
}
