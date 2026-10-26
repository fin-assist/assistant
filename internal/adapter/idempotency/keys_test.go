package idempotency

import (
	"context"
	"errors"
	"fmt"
	"strings"
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

type phaseRefusal struct {
	error
	allowed bool
}

func (e phaseRefusal) Unwrap() error                { return e.error }
func (e phaseRefusal) RequestRetryPermission() bool { return e.allowed }

func TestUnitContractMappingPreservesExplicitPhasePermission(t *testing.T) {
	for _, allowed := range []bool{false, true} {
		original := &asking.RateLimitError{RetryAt: time.Now().Add(time.Hour)}
		phase := phaseRefusal{error: original, allowed: allowed}
		mapped := ContractError(fmt.Errorf("private wrapper: %w", phase))
		rest, ok := finerr.ToREST(mapped)
		if !ok || rest.Code != "RATE_LIMITED" || rest.Metadata["retry_at"] == "" || rest.RequestRetryable == nil || *rest.RequestRetryable != allowed {
			t.Fatalf("phase advice or existing parameters lost: %v", mapped)
		}
		if errors.Is(mapped, phase) || strings.Contains(mapped.Error(), "private") {
			t.Fatal("transport retained the diagnostic wrapper")
		}
	}
	for _, unknown := range []error{errors.New("private storage"), &asking.RateLimitError{RetryAt: time.Now().Add(time.Hour)}} {
		mapped := ContractError(unknown)
		if rest, ok := finerr.ToREST(mapped); ok && rest.RequestRetryable != nil {
			t.Fatalf("unclassified phase acquired advice from its reason: %v", mapped)
		}
	}
	wrapped := phaseRefusal{error: errors.New("private storage"), allowed: true}
	if !errors.Is(ContractError(wrapped), wrapped.error) {
		t.Fatal("unknown plain error became a client-facing status")
	}
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
func TestUnitEffectRefusalsMappedBeforeLibraryClassification(t *testing.T) {
	resets := time.Date(2026, 10, 24, 0, 0, 0, 0, time.FixedZone("MSK", 10800))
	for _, tt := range []struct {
		name   string
		input  error
		reason string
	}{
		{"not found", asking.ErrNotFound, "NOT_FOUND"}, {"retry", asking.ErrRetryDenied, "RETRY_NOT_ALLOWED"}, {"deleted", asking.ErrDeleted, "TOKEN_REVOKED"}, {"consent", asking.ErrConsent, "CONSENT_REQUIRED"}, {"limit", &asking.LimitError{ResetsAt: resets}, "LIMIT_EXCEEDED"},
		{"off-topic", &asking.RateLimitError{RetryAt: resets}, "RATE_LIMITED"},
		{"wrapped off-topic", fmt.Errorf("private storage context: %w", &asking.RateLimitError{RetryAt: resets}), "RATE_LIMITED"},
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
			if tt.reason == "RATE_LIMITED" && (rest.Metadata["retry_at"] != "2026-10-24T00:00:00+03:00" || rest.Spec.HTTPStatus != 429) {
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

func TestUnitOffTopicRetryMetadataNeverRoundsEarlierThanExactExpiry(t *testing.T) {
	for _, fraction := range []time.Duration{0, time.Nanosecond, 900 * time.Millisecond, 999999 * time.Microsecond} {
		exact := time.Now().Add(time.Hour).Truncate(time.Second).Add(fraction)
		mapped := ContractError(&asking.RateLimitError{RetryAt: exact})
		rest, ok := finerr.ToREST(mapped)
		if !ok || rest.Code != "RATE_LIMITED" {
			t.Fatal(mapped)
		}
		advertised, err := time.Parse(time.RFC3339, rest.Metadata["retry_at"])
		if err != nil || advertised.Before(exact) || advertised.Sub(exact) >= time.Second {
			t.Fatalf("advertised %v before/too far from expiry %v: %v", advertised, exact, err)
		}
		if fraction == 0 && !advertised.Equal(exact) {
			t.Fatal("whole second changed")
		}
	}
}
func TestUnitKeysDelegatesEngineResourcesAndErrors(t *testing.T) {
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
