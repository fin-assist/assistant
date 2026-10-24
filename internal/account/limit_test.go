package account

import (
	"testing"
	"time"
)

func TestUnitLimitCalendarAndCounter(t *testing.T) {
	loc := time.FixedZone("MSK", 3*60*60)
	day := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name            string
		now             time.Time
		day             time.Time
		used, max, want int
		reset           string
	}{
		{"new user", time.Date(2026, 10, 23, 20, 59, 59, 0, time.UTC), time.Time{}, 0, 5, 5, "2026-10-24T00:00:00+03:00"},
		{"same day", time.Date(2026, 10, 23, 20, 59, 59, 0, time.UTC), day, 3, 5, 2, "2026-10-24T00:00:00+03:00"},
		{"midnight", time.Date(2026, 10, 23, 21, 0, 0, 0, time.UTC), day, 5, 5, 5, "2026-10-25T00:00:00+03:00"},
		{"clock behind", time.Date(2026, 10, 22, 20, 59, 59, 0, time.UTC), day, 5, 5, 0, "2026-10-24T00:00:00+03:00"},
		{"reduced", time.Date(2026, 10, 23, 0, 0, 0, 0, loc), day, 5, 2, 0, "2026-10-24T00:00:00+03:00"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := CalculateLimit(tt.now, State{LimitDay: tt.day, Used: tt.used}, tt.max)
			if got.Remaining != tt.want || got.ResetsAt.Format(time.RFC3339) != tt.reset {
				t.Fatalf("%+v", got)
			}
		})
	}
}
