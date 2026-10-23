package idempotency

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fin-assist/contracts/gen/go/finerr"
	idem "github.com/fin-assist/go-idempotency"

	"github.com/fin-assist/assistant/internal/asking"
)

type fakeEngine struct {
	result           idem.Result
	err              error
	effectErr        error
	release, reserve bool
}

func (f *fakeEngine) Reserve(context.Context, idem.Request) (idem.Reservation, idem.Result, error) {
	f.reserve = true
	return idem.Reservation{}, f.result, f.err
}
func (f *fakeEngine) WithCompletion(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithCancel(ctx)
}
func (f *fakeEngine) Release(context.Context, idem.Reservation) error { f.release = true; return f.err }
func (f *fakeEngine) DoReserved(ctx context.Context, _ idem.Reservation, fn func(context.Context) (idem.Response, error)) (idem.Result, error) {
	_, f.effectErr = fn(ctx)
	return f.result, f.err
}
func TestEffectRefusalsMappedBeforeLibraryClassification(t *testing.T) {
	resets := time.Date(2026, 10, 24, 0, 0, 0, 0, time.FixedZone("MSK", 10800))
	for _, tt := range []struct {
		name   string
		input  error
		reason string
	}{
		{"not found", asking.ErrNotFound, "NOT_FOUND"}, {"retry", asking.ErrRetryDenied, "RETRY_NOT_ALLOWED"}, {"deleted", asking.ErrDeleted, "TOKEN_REVOKED"}, {"consent", asking.ErrConsent, "CONSENT_REQUIRED"}, {"limit", &asking.LimitError{ResetsAt: resets}, "LIMIT_EXCEEDED"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			engine := &fakeEngine{}
			keys := New(engine)
			_, err := keys.DoReserved(t.Context(), idem.Reservation{}, func(context.Context) (idem.Response, error) { return idem.Response{}, tt.input })
			rest, ok := finerr.ToREST(engine.effectErr)
			if err != nil || !ok || rest.Code != tt.reason {
				t.Fatalf("%v %+v", err, rest)
			}
			if tt.reason == "LIMIT_EXCEEDED" && rest.Metadata["resets_at"] != "2026-10-24T00:00:00+03:00" {
				t.Fatal(rest)
			}
		})
	}
	for _, err := range []error{nil, context.Canceled, asking.ErrUnavailable, errors.New("storage details"), finerr.InternalError()} {
		if !errors.Is(ContractError(err), err) {
			t.Fatalf("unknown error changed: %v", err)
		}
	}
}
func TestKeysDelegatesEngineResourcesAndErrors(t *testing.T) {
	engine := &fakeEngine{err: errors.New("failed"), result: idem.Result{Replayed: true}}
	keys := New(engine)
	if _, result, err := keys.Reserve(t.Context(), idem.Request{}); !engine.reserve || !result.Replayed || !errors.Is(err, engine.err) {
		t.Fatal(err)
	}
	ctx, cancel := keys.WithCompletion(t.Context())
	cancel()
	if ctx.Err() != context.Canceled {
		t.Fatal(ctx.Err())
	}
	if err := keys.Release(t.Context(), idem.Reservation{}); !engine.release || !errors.Is(err, engine.err) {
		t.Fatal(err)
	}
	result, err := keys.DoReserved(t.Context(), idem.Reservation{}, func(context.Context) (idem.Response, error) { return idem.Response{}, nil })
	if !result.Replayed || !errors.Is(err, engine.err) {
		t.Fatal(err)
	}
}
