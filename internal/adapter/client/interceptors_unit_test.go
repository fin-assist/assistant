package client

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/fin-assist/assistant/internal/reqctx"
)

type interceptorToken func(context.Context) (string, error)

func (f interceptorToken) Token(ctx context.Context) (string, error) { return f(ctx) }

func TestUnitOutgoingInterceptorsReplaceSensitiveMetadataAndPreserveDeadlines(t *testing.T) {
	for _, hasDeadline := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "caller"}[hasDeadline], func(t *testing.T) {
			ctx := metadata.NewOutgoingContext(t.Context(), metadata.Pairs("authorization", "Bearer USER-SECRET", "x-client-ip", "client-private", "idempotency-key", "caller-key", "x-service-authorization", "Bearer stale"))
			ctx = reqctx.WithRequestID(ctx, "request-id")
			var callerDeadline time.Time
			if hasDeadline {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, time.Minute)
				defer cancel()
				callerDeadline, _ = ctx.Deadline()
			}
			reg := prometheus.NewRegistry()
			metrics, err := NewMetrics(reg)
			if err != nil {
				t.Fatal(err)
			}
			options := Options{Peer: "analytics", Tokens: DevToken("assistant"), DefaultTimeout: time.Second, Metrics: metrics}
			sentinel := errors.New("peer failure")
			calls := 0
			err = options.unary(ctx, "/analytics.Service/Read", nil, nil, nil, func(actual context.Context, _ string, _, _ any, _ *grpc.ClientConn, _ ...grpc.CallOption) error {
				calls++
				md, _ := metadata.FromOutgoingContext(actual)
				want := metadata.Pairs("x-service-authorization", "Bearer dev:assistant", "x-request-id", "request-id")
				if !reflect.DeepEqual(md, want) {
					t.Fatalf("metadata=%v", md)
				}
				deadline, ok := actual.Deadline()
				if !ok || hasDeadline && !deadline.Equal(callerDeadline) || !hasDeadline && time.Until(deadline) > time.Second {
					t.Fatalf("deadline=%v expected caller=%v", deadline, callerDeadline)
				}
				return sentinel
			})
			if !errors.Is(err, sentinel) || calls != 1 {
				t.Fatalf("calls%d error%v", calls, err)
			}
			families, err := reg.Gather()
			if err != nil || len(families) != 2 {
				t.Fatalf("client metrics=%v error%v", families, err)
			}
			var streamCalled bool
			_, err = options.stream(ctx, &grpc.StreamDesc{}, nil, "/analytics.Service/Watch", func(actual context.Context, _ *grpc.StreamDesc, _ *grpc.ClientConn, _ string, _ ...grpc.CallOption) (grpc.ClientStream, error) {
				streamCalled = true
				_, ok := actual.Deadline()
				if ok != hasDeadline {
					t.Fatalf("stream imposed default deadline=%v", ok)
				}
				md, _ := metadata.FromOutgoingContext(actual)
				if len(md) != 2 || len(md.Get("authorization")) != 0 {
					t.Fatalf("stream leaked metadata=%v", md)
				}
				return nil, sentinel
			})
			if !streamCalled || !errors.Is(err, sentinel) {
				t.Fatalf("stream=%v %v", streamCalled, err)
			}
		})
	}
}

func TestUnitOutgoingTokenFailurePreventsUnaryAndStreamEffects(t *testing.T) {
	sentinel := errors.New("credential unavailable")
	options := Options{Tokens: interceptorToken(func(context.Context) (string, error) { return "", sentinel }), DefaultTimeout: time.Second}
	err := options.unary(t.Context(), "/peer.Service/Read", nil, nil, nil, func(context.Context, string, any, any, *grpc.ClientConn, ...grpc.CallOption) error {
		t.Error("token failure reached invoker")
		return nil
	})
	if !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	_, err = options.stream(t.Context(), nil, nil, "/peer.Service/Watch", func(context.Context, *grpc.StreamDesc, *grpc.ClientConn, string, ...grpc.CallOption) (grpc.ClientStream, error) {
		t.Error("token failure reached streamer")
		return nil, nil
	})
	if !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
}
