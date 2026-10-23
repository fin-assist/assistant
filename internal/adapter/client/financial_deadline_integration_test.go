package client

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fin-assist/assistant/internal/tooldata"
)

func TestFinancialDataRealPeerContextStatusWithLiveCaller(t *testing.T) {
	for name := range financialCalls(nil, tooldata.Subject{}) {
		for _, tc := range []struct {
			code codes.Code
			want error
		}{
			{codes.DeadlineExceeded, context.DeadlineExceeded},
			{codes.Canceled, context.Canceled},
		} {
			t.Run(name+"/"+tc.code.String(), func(t *testing.T) {
				peer := &financialPeer{failure: status.Error(tc.code, "PRIVATE provider status")}
				client := startFinancialPeer(t, peer)
				ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
				defer cancel()
				result, err := financialCalls(client, tooldata.Subject{UserID: "verified", TransferMode: "with"})[name](ctx)
				if ctx.Err() != nil {
					t.Fatalf("caller unexpectedly ended: %v", ctx.Err())
				}
				if !errors.Is(err, tc.want) || err.Error() != tc.want.Error() || len(result.JSON) != 0 {
					t.Fatalf("got result=%+v error=%v; want canonical %v", result, err, tc.want)
				}
				peer.mu.Lock()
				defer peer.mu.Unlock()
				if len(peer.calls) != 1 || peer.calls[name] != 1 {
					t.Fatalf("context status unexpectedly retried: %+v", peer.calls)
				}
			})
		}
	}
}
