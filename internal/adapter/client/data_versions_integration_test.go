package client

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	grpcclient "github.com/fin-assist/go-grpc/client"

	statementv1 "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/fin-assist/go-grpc/reqctx"
)

type versionPeer struct {
	statementv1.UnimplementedAssistantToolsServiceServer
	calls atomic.Int32
	user  chan string
	md    chan metadata.MD
}

func (p *versionPeer) GetDataVersion(ctx context.Context, req *statementv1.GetDataVersionRequest) (*statementv1.GetDataVersionResponse, error) {
	if p.calls.Add(1) == 1 {
		return nil, status.Error(codes.Unavailable, "restarting")
	}
	p.user <- req.UserId
	md, _ := metadata.FromIncomingContext(ctx)
	p.md <- md
	return &statementv1.GetDataVersionResponse{Version: &statementv1.DataVersion{CategoriesChangedAt: &timestamppb.Timestamp{Seconds: 1 << 62}, UploadsChangedAt: &timestamppb.Timestamp{Seconds: 1 << 62}, Revisions: strictWire(9223372036854775807)}}, nil
}
func TestIntegrationDataVersionsRealTransport(t *testing.T) {
	p := &versionPeer{user: make(chan string, 1), md: make(chan metadata.MD, 1)}
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	statementv1.RegisterAssistantToolsServiceServer(server, p)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	conn, err := grpcclient.Dial("passthrough:///statement", grpcclient.Options{Peer: "bank-statement-data", Tokens: grpcclient.DevToken("assistant"), Retry: []string{"finassist.statementdata.v1.AssistantToolsService/GetDataVersion"}, DialOptions: []grpc.DialOption{grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) })}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ctx = reqctx.WithRequestID(ctx, "versions-request")
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer private-user-token")
	result, err := NewDataVersions(statementv1.NewAssistantToolsServiceClient(conn)).Get(ctx, "verified-user")
	if err != nil || result.Revisions["uploads"] != 9223372036854775807 || len(result.Revisions) != 3 || p.calls.Load() != 2 {
		t.Fatalf("%+v %v calls=%d", result, err, p.calls.Load())
	}
	if got := <-p.user; got != "verified-user" {
		t.Fatal(got)
	}
	md := <-p.md
	if got := md.Get("authorization"); len(got) != 0 {
		t.Fatal("forwarded user token")
	}
	if got := md.Get("x-service-authorization"); len(got) != 1 || got[0] != "Bearer dev:assistant" {
		t.Fatal(got)
	}
	if got := md.Get("x-request-id"); len(got) != 1 || got[0] != "versions-request" {
		t.Fatal(got)
	}
}
