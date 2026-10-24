package account

import (
	"context"
	"errors"
	"testing"
	"time"

	assistantv1 "github.com/fin-assist/contracts/gen/go/finassist/assistant/v1"
	deletionv1 "github.com/fin-assist/contracts/gen/go/finassist/userdeletion/v1"
	settingsv1 "github.com/fin-assist/contracts/gen/go/finassist/usersettings/v1"
	"github.com/fin-assist/contracts/gen/go/finerr"

	"github.com/fin-assist/assistant/internal/account"
	"github.com/fin-assist/assistant/internal/reqctx"
)

const id = "00000000-0000-4000-8000-000000000001"

type fakeUseCases struct {
	err        error
	calls      int
	user, zone string
	version    int32
}

func (f *fakeUseCases) GetLimit(_ context.Context, u string) (account.Limit, error) {
	f.calls++
	f.user = u
	return account.Limit{Remaining: 2, DailyMax: 5, ResetsAt: time.Date(2026, 10, 24, 0, 0, 0, 0, time.FixedZone("MSK", 10800))}, f.err
}
func (f *fakeUseCases) SetTimezone(_ context.Context, u, z string, v int32) error {
	f.calls++
	f.user = u
	f.zone = z
	f.version = v
	return f.err
}
func (f *fakeUseCases) Purge(_ context.Context, u string) error { f.calls++; f.user = u; return f.err }
func TestUnitGetLimitSubject(t *testing.T) {
	f := &fakeUseCases{}
	h := &Handler{uc: f}
	for _, u := range []string{"", "invalid"} {
		_, err := h.GetLimit(reqctx.WithUserID(t.Context(), u), &assistantv1.GetLimitRequest{})
		if err == nil || f.calls != 0 {
			t.Fatalf("%v", err)
		}
	}
	got, err := h.GetLimit(reqctx.WithUserID(t.Context(), id), &assistantv1.GetLimitRequest{})
	if err != nil || got.Remaining != 2 || got.DailyMax != 5 || got.TimeZone != "Europe/Moscow" || f.user != id {
		t.Fatalf("%+v %v", got, err)
	}
	f.err = errors.New("failed")
	if _, err := h.GetLimit(reqctx.WithUserID(t.Context(), id), nil); !errors.Is(err, f.err) {
		t.Fatal(err)
	}
}
func TestUnitTimezoneValidationAndDispatch(t *testing.T) {
	for _, tt := range []struct {
		user, zone string
		valid      bool
	}{
		{"", "UTC", false}, {"bad", "UTC", false}, {id, "", false}, {id, "Local", false}, {id, "/UTC", false}, {id, "../UTC", false}, {id, "Unknown/Zone", false}, {id, "UTC", true}, {id, "Asia/Vladivostok", true},
	} {
		t.Run(tt.user+tt.zone, func(t *testing.T) {
			f := &fakeUseCases{}
			h := &Handler{uc: f}
			_, err := h.SetUserTimezone(t.Context(), &settingsv1.SetUserTimezoneRequest{UserId: tt.user, Timezone: tt.zone, SettingsVersion: 2147483647})
			if (err == nil) != tt.valid || f.calls != map[bool]int{true: 1, false: 0}[tt.valid] {
				t.Fatalf("%v calls=%d", err, f.calls)
			}
			if tt.valid && (f.zone != tt.zone || f.version != 2147483647) {
				t.Fatal(f)
			}
		})
	}
	f := &fakeUseCases{err: finerr.InternalError()}
	h := &Handler{uc: f}
	if _, err := h.SetUserTimezone(t.Context(), &settingsv1.SetUserTimezoneRequest{UserId: id, Timezone: "UTC"}); !errors.Is(err, f.err) {
		t.Fatal(err)
	}
	if _, err := h.SetUserTimezone(t.Context(), nil); err == nil {
		t.Fatal("nil request")
	}
}
func TestUnitPurgeValidationAndDispatch(t *testing.T) {
	f := &fakeUseCases{}
	h := &Handler{uc: f}
	for _, u := range []string{"", "bad"} {
		if _, err := h.PurgeUserData(t.Context(), &deletionv1.PurgeUserDataRequest{UserId: u}); err == nil || f.calls != 0 {
			t.Fatal(err)
		}
	}
	if _, err := h.PurgeUserData(t.Context(), &deletionv1.PurgeUserDataRequest{UserId: id}); err != nil || f.user != id {
		t.Fatal(err)
	}
	f.err = errors.New("failed")
	if _, err := h.PurgeUserData(t.Context(), &deletionv1.PurgeUserDataRequest{UserId: id}); !errors.Is(err, f.err) {
		t.Fatal(err)
	}
}
