package app

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"

	assistantv1 "github.com/fin-assist/contracts/gen/go/finassist/assistant/v1"
	deletionv1 "github.com/fin-assist/contracts/gen/go/finassist/userdeletion/v1"
	settingsv1 "github.com/fin-assist/contracts/gen/go/finassist/usersettings/v1"

	"github.com/fin-assist/go-grpc/server"

	"github.com/fin-assist/assistant/internal/logx"
)

// sharedServer builds the go-grpc server the way app.New does, with the service policy and
// handlers that only satisfy registration; identity uses the dev verifiers.
func sharedServer(t *testing.T, log *slog.Logger) *server.Server {
	t.Helper()
	srv, err := server.New(server.Options{}, server.Deps{
		Logger: log, Metrics: prometheus.NewRegistry(), Policy: AccessPolicy(),
		Services: server.DevServiceVerifier{}, Users: server.DevUserVerifier{}, Tombstones: server.NoTombstones{},
	})
	if err != nil {
		t.Fatal(err)
	}
	assistantv1.RegisterAssistantServiceServer(srv.GRPC(), assistantv1.UnimplementedAssistantServiceServer{})
	settingsv1.RegisterUserSettingsSyncServiceServer(srv.GRPC(), settingsv1.UnimplementedUserSettingsSyncServiceServer{})
	deletionv1.RegisterUserDeletionServiceServer(srv.GRPC(), deletionv1.UnimplementedUserDeletionServiceServer{})
	return srv
}

// go-grpc v0.2.0 (FIN-299) turns a ModelMixed call without a verified user token into
// UNAUTHENTICATED and rejects empty caller names. grpc-api.md 3.3 gives assistant no mixed
// method, so the upgrade changes no admission here: the policy must stay free of mixed
// rules and pass the stricter table validation of the shared server.
func TestUnitAccessPolicyPassesSharedServerValidation(t *testing.T) {
	for key, rule := range AccessPolicy() {
		if rule.Model == server.ModelMixed {
			t.Fatalf("%s uses ModelMixed, which grpc-api.md 3.3 does not give assistant", key)
		}
		if len(rule.Callers) == 0 {
			t.Fatalf("%s has no callers", key)
		}
		for _, caller := range rule.Callers {
			if strings.TrimSpace(caller) == "" {
				t.Fatalf("%s has an empty caller name", key)
			}
		}
	}
	if err := sharedServer(t, slog.New(slog.DiscardHandler)).Check(); err != nil {
		t.Fatal(err)
	}
}

// syncBuffer is a log destination safe for the server goroutines writing to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// FIN-300 through go-grpc v0.2.0: the service debug log of a call never carries an
// x-request-id value the server refused (wrong shape, longer than 128 characters); a
// well-formed one stays visible. Real transport on bufconn, the service JSON logger.
func TestIntegrationDebugLogReplacesRejectedRequestIDs(t *testing.T) {
	var logs syncBuffer
	srv := sharedServer(t, logx.New(&logs, "json", slog.LevelDebug, "assistant"))
	lis := bufconn.Listen(1 << 20)
	served := make(chan error, 1)
	go func() { served <- srv.Serve(lis) }()
	t.Cleanup(func() {
		srv.GRPC().Stop()
		if err := <-served; err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			t.Error(err)
		}
	})
	conn, err := grpc.NewClient("passthrough:///assistant",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	health := healthpb.NewHealthClient(conn)
	const valid = "trace-0123456789.abc_DEF-ghi"
	malformed := []string{"PRIVATE request id;drop", strings.Repeat("A", 129)}
	for _, id := range append([]string{valid}, malformed...) {
		ctx := metadata.AppendToOutgoingContext(t.Context(), "x-request-id", id)
		if _, err := health.Check(ctx, &healthpb.HealthCheckRequest{}); err != nil {
			t.Fatal(err)
		}
	}
	out := logs.String()
	if got := strings.Count(out, `"x-request-id":"(rejected)"`); got != len(malformed) {
		t.Fatalf("rejected request IDs logged %d times, want %d:\n%s", got, len(malformed), out)
	}
	for _, id := range malformed {
		if strings.Contains(out, id) {
			t.Fatalf("a rejected x-request-id reached the debug log:\n%s", out)
		}
	}
	if !strings.Contains(out, `"x-request-id":"`+valid+`"`) || !strings.Contains(out, `"request_id":"`+valid+`"`) {
		t.Fatalf("a well-formed x-request-id is no longer logged:\n%s", out)
	}
}
