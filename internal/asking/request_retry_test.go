package asking

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fin-assist/assistant/internal/account"
)

func TestUnitOriginalRequestPermissionBelongsToRefusalPhase(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(setup)
		retry   bool
		allowed *bool
	}{
		{"rolling limit", func(f setup) { f.repo.rateCompleted = repeatedOffTopic(testNow.Add(-time.Hour)) }, false, retryPermission(true)},
		{"locked rolling recheck", func(f setup) {
			f.repo.rateHook = func(_ context.Context) {
				if f.repo.rateCalls == 2 {
					f.repo.rateCompleted = repeatedOffTopic(testNow.Add(-time.Hour))
				}
			}
		}, false, retryPermission(true)},
		{"daily limit", func(f setup) {
			f.repo.state = account.State{LimitDay: account.CalculateLimit(testNow, account.State{}, 5).ResetsAt.AddDate(0, 0, -1), Used: 5}
		}, false, retryPermission(true)},
		{"missing consent", func(f setup) { f.consent.grant = false }, false, retryPermission(false)},
		{"not found before consent", func(f setup) { f.repo.existsErr = ErrNotFound }, true, retryPermission(false)},
		{"retry denied", func(f setup) { f.repo.retryErr = ErrRetryDenied }, true, retryPermission(false)},
		{"deleted", func(f setup) { f.guard.deadAt = 1 }, false, retryPermission(false)},
		{"dependency unavailable", func(f setup) { f.consent.err = errDependency }, false, nil},
		{"disabled model", func(f setup) { f.starter.ready = false }, false, nil},
		{"unknown storage", func(f setup) { f.repo.lockErr = errDependency }, false, nil},
		{"domain error from unknown storage phase", func(f setup) { f.repo.lockErr = &LimitError{ResetsAt: testNow.Add(time.Hour)} }, false, nil},
		{"charge failed", func(f setup) { f.repo.chargeErr = errDependency }, false, nil},
		{"unknown create outcome", func(f setup) { f.repo.createErr = errDependency }, false, nil},
		{"unknown commit outcome", func(f setup) { f.tx.commit = map[int]error{2: errDependency} }, false, nil},
		{"cleanup failure replaces permission", func(f setup) {
			f.repo.rateCompleted = repeatedOffTopic(testNow.Add(-time.Hour))
			f.keys.releaseErr = errDependency
		}, false, nil},
		{"replayed refusal without advice", func(f setup) { f.keys.reserve.Replayed = true; f.keys.reserve.Err = ErrNotFound }, false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := configured(t)
			tc.prepare(f)
			var err error
			if tc.retry {
				_, err = f.s.Retry(t.Context(), user, request(), "answer")
			} else {
				_, err = f.s.Ask(t.Context(), user, request(), "question", "with")
			}
			if err == nil {
				t.Fatal("expected refusal")
			}
			var permission interface{ RequestRetryPermission() bool }
			present := errors.As(err, &permission)
			if tc.allowed == nil {
				if present {
					t.Fatalf("unknown/replayed phase acquired advice: %v", err)
				}
			} else if !present || permission.RequestRetryPermission() != *tc.allowed {
				t.Fatalf("phase permission lost: %v", err)
			}
			if len(f.starter.started) != 0 || f.repo.used != 0 {
				t.Fatal("refusal caused a generation or charge")
			}
		})
	}
}

func retryPermission(value bool) *bool { return &value }
