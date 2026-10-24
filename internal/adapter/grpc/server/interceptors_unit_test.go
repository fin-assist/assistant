package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/fin-assist/contracts/gen/go/finerr"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	testgrpc "google.golang.org/grpc/interop/grpc_testing"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/fin-assist/assistant/internal/logx"
	"github.com/fin-assist/assistant/internal/reqctx"
)

func isolatedServer(t *testing.T, p Policy, adjust ...func(*Deps)) (*Server, *bytes.Buffer) {
	t.Helper()
	logs := &bytes.Buffer{}
	deps := Deps{Logger: logx.New(logs, "json", slog.LevelDebug, "test"), Metrics: prometheus.NewRegistry(), Policy: p,
		Services: DevServiceVerifier{}, Users: DevUserVerifier{}, Tombstones: deleted{"gone": true}}
	for _, mutate := range adjust {
		mutate(&deps)
	}
	srv, err := New(Options{Reflection: true}, deps)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.grpc.Stop)
	return srv, logs
}

func incoming(t *testing.T, caller, user string, hints ...string) context.Context {
	t.Helper()
	ctx := call(t, caller, user, hints...)
	md, _ := metadata.FromOutgoingContext(ctx)
	return metadata.NewIncomingContext(ctx, md)
}

func TestUnitUnaryAdmissionBeforeEffects(t *testing.T) {
	for _, tc := range []struct {
		name, method, caller, user, reason string
		code                               codes.Code
		want                               seen
	}{
		{name: "user", method: emptyCall, caller: "bank-bff", user: "u1", want: seen{user: "u1", hasUser: true, caller: "bank-bff", ip: "203.0.113.9", version: "android/test", readPrimary: true}},
		{name: "missing-service", method: emptyCall, code: codes.Unauthenticated},
		{name: "foreign-service", method: emptyCall, caller: "analytics", user: "u1", code: codes.PermissionDenied},
		{name: "missing-user", method: emptyCall, caller: "bank-bff", code: codes.Unauthenticated, reason: "INVALID_TOKEN"},
		{name: "deleted-user", method: emptyCall, caller: "bank-bff", user: "gone", code: codes.Unauthenticated, reason: "TOKEN_REVOKED"},
		{name: "tombstone-unavailable", method: emptyCall, caller: "bank-bff", user: "broken", code: codes.Unavailable},
		{name: "mixed-system", method: unaryCall, caller: "assistant", want: seen{caller: "assistant"}},
		{name: "mixed-user", method: unaryCall, caller: "assistant", user: "u1", want: seen{caller: "assistant", user: "u1", hasUser: true}},
		{name: "system-ignores-user-and-hints", method: streamOut, caller: "user", user: "gone", want: seen{caller: "user"}},
		{name: "no-user", method: "/grpc.testing.TestService/Nop", caller: "user", user: "gone", want: seen{caller: "user"}},
		{name: "open-health", method: "/grpc.health.v1.Health/Check"},
		{name: "unlisted-method", method: "/other.Service/Method", caller: "user", code: codes.PermissionDenied},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := isolatedServer(t, policy())
			called := false
			resp, err := srv.unary(incoming(t, tc.caller, tc.user, "x-client-ip", "203.0.113.9", "x-client-version", "android/test", "x-read-primary", "1", "x-request-id", "unsafe\nrequest"), &testgrpc.Empty{}, &grpc.UnaryServerInfo{FullMethod: tc.method}, func(ctx context.Context, _ any) (any, error) {
				called = true
				if got := record(ctx); got != tc.want {
					t.Errorf("handler identity %+v, want %+v", got, tc.want)
				}
				if id := reqctx.RequestID(ctx); len(id) != 32 || strings.Contains(id, "unsafe") {
					t.Errorf("invalid request ID %q", id)
				}
				return "accepted", nil
			})
			if status.Code(err) != tc.code || Reason(err) != tc.reason {
				t.Fatalf("error=%v reason=%s", err, Reason(err))
			}
			if called != (tc.code == codes.OK) {
				t.Fatalf("handler called=%v after admission %v", called, err)
			}
			if called && resp != "accepted" {
				t.Fatalf("response=%v", resp)
			}
		})
	}
}

func TestUnitUnaryRejectsInvalidTokensAndMessages(t *testing.T) {
	srv, _ := isolatedServer(t, policy())
	for _, md := range []metadata.MD{
		metadata.Pairs(mdServiceAuthorization, "Bearer invalid"),
		metadata.Pairs(mdServiceAuthorization, "Bearer dev:bank-bff", mdAuthorization, "Bearer invalid"),
	} {
		_, err := srv.unary(metadata.NewIncomingContext(t.Context(), md), &testgrpc.Empty{}, &grpc.UnaryServerInfo{FullMethod: emptyCall}, func(context.Context, any) (any, error) { t.Error("invalid token reached handler"); return nil, nil })
		if status.Code(err) != codes.Unauthenticated {
			t.Fatal(err)
		}
	}
	for name, msg := range map[string]any{"bad-request": validationMessage(t), "broken-rule": brokenRuleMessage(t)} {
		t.Run(name, func(t *testing.T) {
			_, err := srv.unary(incoming(t, "bank-bff", "u1", "x-request-id", "valid-request"), msg, &grpc.UnaryServerInfo{FullMethod: emptyCall}, func(context.Context, any) (any, error) { t.Error("invalid message reached handler"); return nil, nil })
			want := "VALIDATION_ERROR"
			if name == "broken-rule" {
				want = "INTERNAL_ERROR"
			}
			if Reason(err) != want {
				t.Fatalf("got %v reason %s", err, Reason(err))
			}
		})
	}
	subjectlessServer, _ := isolatedServer(t, policy(), func(d *Deps) { d.Users = subjectless{} })
	_, err := subjectlessServer.authorize(incoming(t, "bank-bff", "u1"), emptyCall)
	if Reason(err) != "INVALID_TOKEN" {
		t.Fatal(err)
	}
	invalidPolicy := policy()
	invalidPolicy[emptyCall] = Rule{Callers: []string{"bank-bff"}}
	invalidServer, _ := isolatedServer(t, invalidPolicy)
	if _, err := invalidServer.authorize(incoming(t, "bank-bff", "u1"), emptyCall); status.Code(err) != codes.PermissionDenied {
		t.Fatal(err)
	}
}

type isolatedStream struct {
	grpc.ServerStream
	ctx     context.Context
	recvErr error
}

func (s *isolatedStream) Context() context.Context { return s.ctx }
func (s *isolatedStream) RecvMsg(any) error        { return s.recvErr }

func TestUnitStreamAdmissionValidationAndTransportFailure(t *testing.T) {
	srv, _ := isolatedServer(t, policy())
	denied := &isolatedStream{ctx: t.Context()}
	err := srv.stream(nil, denied, &grpc.StreamServerInfo{FullMethod: streamOut}, func(any, grpc.ServerStream) error { t.Error("unauthenticated stream handler called"); return nil })
	if status.Code(err) != codes.Unauthenticated {
		t.Fatal(err)
	}
	for _, recvErr := range []error{nil, io.EOF, context.Canceled} {
		stream := &isolatedStream{ctx: incoming(t, "user", "gone"), recvErr: recvErr}
		err := srv.stream(nil, stream, &grpc.StreamServerInfo{FullMethod: streamOut}, func(_ any, ss grpc.ServerStream) error {
			if got := record(ss.Context()); got != (seen{caller: "user"}) {
				t.Errorf("stream identity=%+v", got)
			}
			return ss.RecvMsg(validationMessage(t))
		})
		if recvErr == nil {
			if Reason(err) != "VALIDATION_ERROR" {
				t.Fatal(err)
			}
		} else if !errors.Is(err, recvErr) && status.Code(err) != status.Code(normalize(recvErr)) {
			t.Fatalf("receive error %v became %v", recvErr, err)
		}
	}
}

func TestUnitInterceptorsRecoverAndNormalizeHandlerFailures(t *testing.T) {
	for _, cause := range []error{errors.New("private SQL details"), finerr.NotFound()} {
		srv, logs := isolatedServer(t, policy())
		_, err := srv.unary(incoming(t, "bank-bff", "u1"), &testgrpc.Empty{}, &grpc.UnaryServerInfo{FullMethod: emptyCall}, func(context.Context, any) (any, error) { return nil, cause })
		if status.Code(err) != status.Code(normalize(cause)) || strings.Contains(status.Convert(err).Message(), "private") {
			t.Fatalf("unsafe handler error=%v", err)
		}
		if !strings.Contains(logs.String(), cause.Error()) {
			t.Fatalf("cause absent from internal log: %s", logs)
		}
	}
	srv, _ := isolatedServer(t, policy())
	_, err := srv.unary(incoming(t, "bank-bff", "u1"), nil, &grpc.UnaryServerInfo{FullMethod: emptyCall}, func(context.Context, any) (any, error) { panic("private panic") })
	if status.Code(err) != codes.Internal || len(status.Convert(err).Details()) != 0 {
		t.Fatalf("unary panic=%v", err)
	}
	err = srv.stream(nil, &isolatedStream{ctx: incoming(t, "user", "")}, &grpc.StreamServerInfo{FullMethod: streamOut}, func(any, grpc.ServerStream) error { panic("private panic") })
	if status.Code(err) != codes.Internal || len(status.Convert(err).Details()) != 0 {
		t.Fatalf("stream panic=%v", err)
	}
}
