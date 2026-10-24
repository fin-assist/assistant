package client

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type consentClient struct {
	userv1.UserInternalServiceClient
	response *userv1.GetAssistantConsentStatusResponse
	err      error
	calls    int
	user     *string
	call     func()
	context  context.Context
}

func (c *consentClient) GetAssistantConsentStatus(ctx context.Context, r *userv1.GetAssistantConsentStatusRequest, _ ...grpc.CallOption) (*userv1.GetAssistantConsentStatusResponse, error) {
	c.calls++
	c.user = r.UserId
	c.context = ctx
	if c.call != nil {
		c.call()
	}
	return c.response, c.err
}

func TestUnitAssistantConsentCapsReadDeadline(t *testing.T) {
	for _, parentTimeout := range []time.Duration{3 * time.Minute, time.Second} {
		t.Run(parentTimeout.String(), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), parentTimeout)
			defer cancel()
			version := "current"
			peer := &consentClient{response: consentResponse(true, &version, version)}
			start := time.Now()
			granted, err := NewAssistantConsent(peer).Granted(ctx, "user")
			if !granted || err != nil {
				t.Fatalf("grant=%v error=%v", granted, err)
			}
			deadline, ok := peer.context.Deadline()
			if !ok {
				t.Fatal("consent read has no deadline")
			}
			if parentTimeout > 5*time.Second {
				if elapsed := deadline.Sub(start); elapsed < 4*time.Second || elapsed > 6*time.Second {
					t.Fatalf("consent deadline duration=%v", elapsed)
				}
			} else if original, _ := ctx.Deadline(); !deadline.Equal(original) {
				t.Fatalf("short parent deadline extended: got=%v want=%v", deadline, original)
			}
			if !errors.Is(peer.context.Err(), context.Canceled) {
				t.Fatal("consent timeout resources not released after success")
			}
			if ctx.Err() != nil {
				t.Fatal("read cancelled its parent context")
			}
		})
	}
}

func consentResponse(granted bool, version *string, current string) *userv1.GetAssistantConsentStatusResponse {
	return &userv1.GetAssistantConsentStatusResponse{Consent: &userv1.AssistantConsent{Granted: granted, Version: version, CurrentVersion: current}}
}

func TestUnitAssistantConsentResponse(t *testing.T) {
	current, stale, empty := "2026-10-24", "2026-09-28", ""
	for _, tc := range []struct {
		name             string
		response         *userv1.GetAssistantConsentStatusResponse
		granted, invalid bool
	}{
		{name: "current", response: consentResponse(true, &current, current), granted: true},
		{name: "not granted", response: consentResponse(false, nil, current)},
		{name: "revoked explicit flag", response: consentResponse(false, &current, current)},
		{name: "outdated grant", response: consentResponse(true, &stale, current)},
		{name: "outdated inactive grant", response: consentResponse(false, &stale, current)},
		{name: "different opaque version", response: consentResponse(true, &current, stale)},
		{name: "nil response", invalid: true},
		{name: "nil consent", response: &userv1.GetAssistantConsentStatusResponse{}, invalid: true},
		{name: "missing current", response: consentResponse(false, nil, ""), invalid: true},
		{name: "missing current active", response: consentResponse(true, &current, ""), invalid: true},
		{name: "missing version", response: consentResponse(true, nil, current), invalid: true},
		{name: "empty version", response: consentResponse(true, &empty, current), invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			peer := &consentClient{response: tc.response}
			granted, err := NewAssistantConsent(peer).Granted(t.Context(), "verified-user")
			if granted != tc.granted || (err != nil) != tc.invalid {
				t.Fatalf("granted=%v err=%v", granted, err)
			}
			if peer.calls != 1 || peer.user == nil || *peer.user != "verified-user" {
				t.Fatalf("calls=%d user=%v", peer.calls, peer.user)
			}
		})
	}
}

func TestUnitAssistantConsentUnconfiguredAndInvalidUser(t *testing.T) {
	if granted, err := NewAssistantConsent(nil).Granted(t.Context(), "user"); granted || err == nil {
		t.Fatalf("unconfigured granted=%v err=%v", granted, err)
	}
	for _, user := range []string{"", " \t\n"} {
		peer := &consentClient{}
		if granted, err := NewAssistantConsent(peer).Granted(t.Context(), user); granted || err == nil {
			t.Fatalf("empty user granted=%v err=%v", granted, err)
		}
		if peer.calls != 0 {
			t.Fatal("invalid user reached peer")
		}
	}
}

func TestUnitAssistantConsentDoesNotCache(t *testing.T) {
	v := "v1"
	peer := &consentClient{response: consentResponse(true, &v, v)}
	c := NewAssistantConsent(peer)
	if granted, err := c.Granted(t.Context(), "user"); !granted || err != nil {
		t.Fatalf("grant=%v err=%v", granted, err)
	}
	peer.response = consentResponse(false, nil, v)
	if granted, err := c.Granted(t.Context(), "user"); granted || err != nil {
		t.Fatalf("revoked=%v err=%v", granted, err)
	}
	if peer.calls != 2 {
		t.Fatalf("cached grant: calls=%d", peer.calls)
	}
}

func TestUnitAssistantConsentSanitizesErrors(t *testing.T) {
	for _, raw := range []error{errors.New("private-user private-token SQL"), status.Error(codes.Unavailable, "private-user private-token SQL"), status.Error(codes.PermissionDenied, "private-user private-token SQL")} {
		peer := &consentClient{err: raw}
		granted, err := NewAssistantConsent(peer).Granted(t.Context(), "private-user")
		if granted || err == nil || errors.Is(err, raw) {
			t.Fatalf("unsanitized grant=%v err=%v", granted, err)
		}
		for _, private := range []string{"private-user", "private-token", "SQL"} {
			if strings.Contains(err.Error(), private) {
				t.Fatalf("private data in error: %v", err)
			}
		}
	}
}

func TestUnitAssistantConsentPreservesContextErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		peer error
		want error
	}{
		{"cancelled", context.Canceled, context.Canceled},
		{"wrapped deadline", fmt.Errorf("private peer: %w", context.DeadlineExceeded), context.DeadlineExceeded},
		{"grpc cancelled", status.Error(codes.Canceled, "private peer"), context.Canceled},
		{"grpc deadline", status.Error(codes.DeadlineExceeded, "private peer"), context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			granted, err := NewAssistantConsent(&consentClient{err: tc.peer}).Granted(t.Context(), "user")
			if granted || !errors.Is(err, tc.want) {
				t.Fatalf("granted=%v error=%v want=%v", granted, err, tc.want)
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	peer := &consentClient{}
	if granted, err := NewAssistantConsent(peer).Granted(ctx, "user"); granted || !errors.Is(err, context.Canceled) || peer.calls != 0 {
		t.Fatalf("cancelled admission granted=%v err=%v calls=%d", granted, err, peer.calls)
	}
	ctx, cancel = context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	if granted, err := NewAssistantConsent(peer).Granted(ctx, "user"); granted || !errors.Is(err, context.DeadlineExceeded) || peer.calls != 0 {
		t.Fatalf("expired admission granted=%v err=%v calls=%d", granted, err, peer.calls)
	}
	ctx, cancel = context.WithCancel(t.Context())
	defer cancel()
	v := "v1"
	peer = &consentClient{response: consentResponse(true, &v, v), call: cancel}
	if granted, err := NewAssistantConsent(peer).Granted(ctx, "user"); granted || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled response granted=%v err=%v", granted, err)
	}
}
