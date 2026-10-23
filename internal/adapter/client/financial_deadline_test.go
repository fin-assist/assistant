package client

import (
	"context"
	"fmt"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fin-assist/assistant/internal/tooldata"
)

func TestFinancialDataPeerContextStatusWithLiveCaller(t *testing.T) {
	for name := range financialCalls(nil, tooldata.Subject{}) {
		for _, tc := range []struct {
			name string
			peer error
			want error
		}{
			{"deadline", status.Error(codes.DeadlineExceeded, "PRIVATE provider deadline"), context.DeadlineExceeded},
			{"canceled", status.Error(codes.Canceled, "PRIVATE provider cancellation"), context.Canceled},
			{"wrapped deadline", fmt.Errorf("PRIVATE wrapper: %w", status.Error(codes.DeadlineExceeded, "PRIVATE provider deadline")), context.DeadlineExceeded},
			{"unavailable", status.Error(codes.Unavailable, "PRIVATE provider unavailable"), tooldata.ErrUnavailable},
			{"internal", status.Error(codes.Internal, "PRIVATE provider internal"), tooldata.ErrUnavailable},
		} {
			t.Run(name+"/"+tc.name, func(t *testing.T) {
				c, peer, subject := financialFixture(t)
				peer.err = tc.peer
				ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
				defer cancel()
				result, err := financialCalls(c, subject)[name](ctx)
				if ctx.Err() != nil {
					t.Fatalf("caller unexpectedly ended: %v", ctx.Err())
				}
				if err != tc.want || len(result.JSON) != 0 {
					t.Fatalf("got result=%+v error=%v; want canonical %v", result, err, tc.want)
				}
			})
		}
	}
}

func TestFinancialDataCallerContextErrorTakesPriority(t *testing.T) {
	for name := range financialCalls(nil, tooldata.Subject{}) {
		t.Run(name, func(t *testing.T) {
			c, peer, subject := financialFixture(t)
			call := financialCalls(c, subject)[name]
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			peer.err = status.Error(codes.DeadlineExceeded, "PRIVATE peer deadline")
			if _, err := call(ctx); err != context.Canceled {
				t.Fatalf("canceled caller lost priority: %v", err)
			}
			ctx, cancel = context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
			defer cancel()
			peer.err = status.Error(codes.Canceled, "PRIVATE peer canceled")
			if _, err := call(ctx); err != context.DeadlineExceeded {
				t.Fatalf("expired caller lost priority: %v", err)
			}
		})
	}
}
