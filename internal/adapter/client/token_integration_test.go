package client

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/grpc"
	testgrpc "google.golang.org/grpc/interop/grpc_testing"
	"google.golang.org/grpc/test/bufconn"
)

func TestIntegrationProjectedServiceTokenRotationReachesPeerWithoutReconnect(t *testing.T) {
	path := filepath.Join(t.TempDir(), "projected-token")
	replace := func(token string) {
		t.Helper()
		next := path + ".next"
		if err := os.WriteFile(next, []byte(token), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(next, path); err != nil {
			t.Fatal(err)
		}
	}
	replace(" first-service-token\n")
	p := newPeer()
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	testgrpc.RegisterTestServiceServer(server, p)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	conn, err := Dial("passthrough:///token-peer", Options{Peer: "analytics", Tokens: &FileToken{Path: path}, DialOptions: []grpc.DialOption{grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) })}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	c := testgrpc.NewTestServiceClient(conn)
	assertToken := func(want string) {
		t.Helper()
		if _, err := c.EmptyCall(t.Context(), &testgrpc.Empty{}); err != nil {
			t.Fatal(err)
		}
		md := <-p.md
		<-p.hasDeadline
		if got := md.Get("x-service-authorization"); len(got) != 1 || got[0] != "Bearer "+want {
			t.Fatalf("peer token=%v want%s", got, want)
		}
		if len(md.Get("authorization")) != 0 {
			t.Fatal("service token was sent as user credential")
		}
	}
	assertToken("first-service-token")
	assertToken("first-service-token")
	replace("second-rotated-service-token")
	assertToken("second-rotated-service-token")
	for _, state := range []string{"empty", "directory", "missing"} {
		t.Run(state, func(t *testing.T) {
			switch state {
			case "empty":
				replace(" \n")
			case "directory":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			case "missing":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			before := p.calls.Load()
			if _, err := c.EmptyCall(t.Context(), &testgrpc.Empty{}); err == nil {
				t.Fatal("unavailable projected credential reused cached token")
			}
			if p.calls.Load() != before {
				t.Fatal("invalid credential reached peer")
			}
		})
	}
	replace("recovered-service-token-after-rotation")
	assertToken("recovered-service-token-after-rotation")
	if p.calls.Load() != 4 {
		t.Fatalf("peer received%d calls want4", p.calls.Load())
	}
}
