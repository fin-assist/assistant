package client

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/fin-assist/assistant/internal/reqctx"
)

type consentCall struct {
	user     *string
	md       metadata.MD
	deadline bool
}

type consentPeer struct {
	userv1.UnimplementedUserInternalServiceServer
	read     func(context.Context, *userv1.GetAssistantConsentStatusRequest) (*userv1.GetAssistantConsentStatusResponse, error)
	calls    atomic.Int32
	keys     atomic.Int32
	observed chan consentCall
}

func (p *consentPeer) GetAssistantConsentStatus(ctx context.Context, r *userv1.GetAssistantConsentStatusRequest) (*userv1.GetAssistantConsentStatusResponse, error) {
	p.calls.Add(1)
	md, _ := metadata.FromIncomingContext(ctx)
	_, deadline := ctx.Deadline()
	select {
	case p.observed <- consentCall{user: r.UserId, md: md, deadline: deadline}:
	default:
	}
	return p.read(ctx, r)
}

func (p *consentPeer) GetSigningKeys(context.Context, *userv1.GetSigningKeysRequest) (*userv1.GetSigningKeysResponse, error) {
	p.keys.Add(1)
	return nil, status.Error(codes.Unavailable, "private-user private-token signing keys")
}

func dialConsent(t *testing.T, p *consentPeer, timeout time.Duration) (*AssistantConsent, userv1.UserInternalServiceClient) {
	t.Helper()
	p.observed = make(chan consentCall, 16)
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	userv1.RegisterUserInternalServiceServer(server, p)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	t.Cleanup(func() { _ = listener.Close() })
	conn, err := Dial("passthrough:///consent", Options{
		Peer: "user", Tokens: DevToken("assistant"), DefaultTimeout: timeout,
		Retry:       []string{"finassist.user.v1.UserInternalService/GetAssistantConsentStatus"},
		DialOptions: []grpc.DialOption{grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) })},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	client := userv1.NewUserInternalServiceClient(conn)
	return NewAssistantConsent(client), client
}

func TestAssistantConsentRealTransportRetryMetadataAndRevocation(t *testing.T) {
	version := "opaque-current"
	var revoked atomic.Bool
	var failures atomic.Int32
	failures.Store(2)
	p := &consentPeer{read: func(context.Context, *userv1.GetAssistantConsentStatusRequest) (*userv1.GetAssistantConsentStatusResponse, error) {
		if failures.Add(-1) >= 0 {
			return nil, status.Error(codes.Unavailable, "private-user private-token restarting")
		}
		if revoked.Load() {
			return consentResponse(false, nil, version), nil
		}
		return consentResponse(true, &version, version), nil
	}}
	c, raw := dialConsent(t, p, time.Second)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ctx = reqctx.WithRequestID(ctx, "consent-request")
	ctx = metadata.NewIncomingContext(ctx, metadata.Pairs("authorization", "Bearer private-user-token", "x-client-ip", "192.0.2.1"))
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer private-user-token", "x-service-authorization", "Bearer foreign-service", "x-client-ip", "192.0.2.1", "x-client-version", "1.2.3", "x-read-primary", "false", "x-user-id", "foreign-user")
	granted, err := c.Granted(ctx, "verified-user")
	if !granted || err != nil || p.calls.Load() != 3 {
		t.Fatalf("grant=%v err=%v calls=%d", granted, err, p.calls.Load())
	}
	revoked.Store(true)
	granted, err = c.Granted(ctx, "verified-user")
	if granted || err != nil || p.calls.Load() != 4 {
		t.Fatalf("revoked grant=%v err=%v calls=%d", granted, err, p.calls.Load())
	}
	for range 4 {
		select {
		case call := <-p.observed:
			if call.user == nil || *call.user != "verified-user" || !call.deadline {
				t.Fatalf("system request=%+v", call)
			}
			for header, want := range map[string]string{"x-service-authorization": "Bearer dev:assistant", "x-request-id": "consent-request"} {
				got := call.md.Get(header)
				if len(got) != 1 || got[0] != want {
					t.Fatalf("%s=%v", header, got)
				}
			}
			for _, header := range []string{"authorization", "x-client-ip", "x-client-version", "x-read-primary", "x-user-id"} {
				if got := call.md.Get(header); len(got) != 0 {
					t.Fatalf("forwarded %s=%v", header, got)
				}
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	// The retry entry names only this consent read; it does not silently enable
	// retries for every RPC on the user connection.
	if _, err = raw.GetSigningKeys(ctx, &userv1.GetSigningKeysRequest{}); status.Code(err) != codes.Unavailable || p.keys.Load() != 1 {
		t.Fatalf("unlisted method error=%v attempts=%d", err, p.keys.Load())
	}
}

func TestAssistantConsentRealTransportRefusals(t *testing.T) {
	for _, code := range []codes.Code{codes.Unavailable, codes.Internal, codes.PermissionDenied} {
		t.Run(code.String(), func(t *testing.T) {
			p := &consentPeer{read: func(context.Context, *userv1.GetAssistantConsentStatusRequest) (*userv1.GetAssistantConsentStatusResponse, error) {
				return nil, status.Error(code, "private-user private-token peer-details")
			}}
			c, _ := dialConsent(t, p, time.Second)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			granted, err := c.Granted(ctx, "private-user")
			if granted || err == nil {
				t.Fatalf("grant=%v err=%v", granted, err)
			}
			for _, private := range []string{"private-user", "private-token", "peer-details"} {
				if strings.Contains(err.Error(), private) {
					t.Fatalf("unsanitized error=%v", err)
				}
			}
			want := int32(1)
			if code == codes.Unavailable {
				want = 3
			}
			if p.calls.Load() != want {
				t.Fatalf("attempts=%d want=%d", p.calls.Load(), want)
			}
		})
	}
}

func TestAssistantConsentRealTransportInvalidResponse(t *testing.T) {
	version := "current"
	for _, response := range []*userv1.GetAssistantConsentStatusResponse{{}, consentResponse(true, nil, version), consentResponse(true, &version, "")} {
		p := &consentPeer{read: func(context.Context, *userv1.GetAssistantConsentStatusRequest) (*userv1.GetAssistantConsentStatusResponse, error) {
			return response, nil
		}}
		c, _ := dialConsent(t, p, time.Second)
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		granted, err := c.Granted(ctx, "user")
		cancel()
		if granted || err == nil || p.calls.Load() != 1 {
			t.Fatalf("invalid response grant=%v err=%v calls=%d", granted, err, p.calls.Load())
		}
	}
}

func TestAssistantConsentRealTransportCancellation(t *testing.T) {
	started := make(chan struct{})
	ended := make(chan struct{})
	p := &consentPeer{read: func(ctx context.Context, _ *userv1.GetAssistantConsentStatusRequest) (*userv1.GetAssistantConsentStatusResponse, error) {
		close(started)
		<-ctx.Done()
		close(ended)
		return nil, ctx.Err()
	}}
	c, _ := dialConsent(t, p, 5*time.Second)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		granted, err := c.Granted(ctx, "user")
		if granted {
			done <- errors.New("cancelled grant accepted")
			return
		}
		done <- err
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation lost: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled read did not stop")
	}
	select {
	case <-ended:
	case <-time.After(time.Second):
		t.Fatal("peer did not receive cancellation")
	}
	if p.calls.Load() != 1 {
		t.Fatalf("cancelled read retried: %d", p.calls.Load())
	}
}

func TestAssistantConsentRealTransportDefaultDeadline(t *testing.T) {
	ended := make(chan struct{})
	p := &consentPeer{read: func(ctx context.Context, _ *userv1.GetAssistantConsentStatusRequest) (*userv1.GetAssistantConsentStatusResponse, error) {
		<-ctx.Done()
		close(ended)
		return nil, ctx.Err()
	}}
	c, _ := dialConsent(t, p, time.Second)
	// The original context has no deadline: the consent adapter caps the read.
	// The transport's deadline failure remains context.DeadlineExceeded.
	granted, err := c.Granted(t.Context(), "user")
	if granted || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("default deadline grant=%v err=%v", granted, err)
	}
	select {
	case <-ended:
	case <-time.After(time.Second):
		t.Fatal("peer did not receive deadline")
	}
	if p.calls.Load() != 1 {
		t.Fatalf("expired read retried: %d", p.calls.Load())
	}
}

func TestAssistantConsentRealTransportCapsLongParentDeadline(t *testing.T) {
	for _, parentTimeout := range []time.Duration{3 * time.Minute, time.Second} {
		t.Run(parentTimeout.String(), func(t *testing.T) {
			deadline := make(chan time.Time, 1)
			version := "current"
			p := &consentPeer{read: func(ctx context.Context, _ *userv1.GetAssistantConsentStatusRequest) (*userv1.GetAssistantConsentStatusResponse, error) {
				at, _ := ctx.Deadline()
				deadline <- at
				return consentResponse(true, &version, version), nil
			}}
			c, _ := dialConsent(t, p, 3*time.Minute)
			ctx, cancel := context.WithTimeout(t.Context(), parentTimeout)
			defer cancel()
			start := time.Now()
			granted, err := c.Granted(ctx, "user")
			if !granted || err != nil {
				t.Fatalf("grant=%v err=%v", granted, err)
			}
			select {
			case at := <-deadline:
				elapsed := at.Sub(start)
				maximum := 6 * time.Second
				if parentTimeout < 5*time.Second {
					maximum = parentTimeout + 100*time.Millisecond
				}
				if at.IsZero() || elapsed <= 0 || elapsed > maximum {
					t.Fatalf("server deadline duration=%v max=%v", elapsed, maximum)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		})
	}
}
