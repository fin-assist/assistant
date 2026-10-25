package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	assistantv1 "github.com/fin-assist/contracts/gen/go/finassist/assistant/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	settingsv1 "github.com/fin-assist/contracts/gen/go/finassist/usersettings/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/fin-assist/assistant/internal/testdb"
)

// The real command loads environment/file configuration and serves the public API
// over TCP; an in-process handler or a configuration-only check cannot satisfy this test.
func TestAPICommandServesConfiguredLimitsReadinessAndHistoryThenStops(t *testing.T) {
	dsn := testdb.DSN(t)
	dsnFile := filepath.Join(t.TempDir(), "primary-dsn")
	if err := os.WriteFile(dsnFile, []byte(dsn), 0600); err != nil {
		t.Fatal(err)
	}
	grpcReservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = grpcReservation.Close() }()
	httpReservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = httpReservation.Close() }()
	grpcAddress, httpAddress := grpcReservation.Addr().String(), httpReservation.Addr().String()
	for name, value := range map[string]string{
		"SERVICE_NAME": "assistant", "ENV": "dev", "AUTH_MODE": "insecure-dev",
		"LOG_LEVEL": "info", "LOG_FORMAT": "json", "ASSISTANT_DAILY_MAX": "7",
		"DB_PRIMARY_DSN": "", "DB_PRIMARY_DSN_FILE": dsnFile,
		"GRPC_ADDR": grpcAddress, "HTTP_ADDR": httpAddress,
		"SHUTDOWN_DELAY": "0s", "SHUTDOWN_TIMEOUT": "5s",
		"ASSISTANT_MODEL_URI": "", "PEER_USER_ADDR": "", "PEER_ANALYTICS_ADDR": "", "PEER_BANK_STATEMENT_DATA_ADDR": "",
	} {
		t.Setenv(name, value)
	}
	previousLogger := slog.Default()
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	// Apply the same deployment command an operator runs before starting the service.
	if err := run(t.Context(), []string{"migrate", "up"}); err != nil {
		t.Fatal(err)
	}
	_ = grpcReservation.Close()
	_ = httpReservation.Close()
	serveContext, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- run(serveContext, []string{"serve"}) }()
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("serve: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Error("serve did not stop after cancellation")
			}
		})
	}
	t.Cleanup(stop)
	conn, err := grpc.NewClient(grpcAddress, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	requestContext, requestCancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer requestCancel()
	healthContext, healthCancel := context.WithCancel(requestContext)
	defer healthCancel()
	watch, err := healthpb.NewHealthClient(conn).Watch(healthContext, &healthpb.HealthCheckRequest{}, grpc.WaitForReady(true))
	if err != nil {
		t.Fatal(err)
	}
	for {
		event, err := watch.Recv()
		if err != nil {
			t.Fatalf("health never became serving: %v", err)
		}
		if event.Status == healthpb.HealthCheckResponse_SERVING {
			break
		}
	}
	healthCancel()
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	httpClient := &http.Client{Transport: transport, Timeout: time.Second}
	readyURL := "http://" + httpAddress + "/readyz"
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		code, body, err := commandAPIGet(requestContext, httpClient, readyURL)
		if err == nil && code == http.StatusOK && body == "ok\n" {
			break
		}
		select {
		case <-requestContext.Done():
			t.Fatalf("HTTP readiness never became ready: status=%d body=%q error=%v", code, body, err)
		case <-tick.C:
		}
	}
	for path, want := range map[string]string{"/healthz": "ok\n", "/metrics": "go_goroutines"} {
		code, body, err := commandAPIGet(requestContext, httpClient, "http://"+httpAddress+path)
		if err != nil || code != http.StatusOK || !strings.Contains(body, want) {
			t.Fatalf("operational endpoint %s: status=%d body=%q error=%v", path, code, body, err)
		}
	}
	userContext := metadata.NewOutgoingContext(requestContext, metadata.Pairs("x-service-authorization", "Bearer dev:bank-bff", "authorization", "Bearer dev:10000000-0000-4000-8000-000000000001"))
	client := assistantv1.NewAssistantServiceClient(conn)
	if _, err := client.GetLimit(requestContext, &assistantv1.GetLimitRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("real command exposed a user method without identity: %v", err)
	}
	limit, err := client.GetLimit(userContext, &assistantv1.GetLimitRequest{})
	if err != nil || limit.GetDailyMax() != 7 || limit.GetRemaining() != 7 || limit.GetTimeZone() != "Europe/Moscow" {
		t.Fatalf("configured limit over TCP: %v %v", limit, err)
	}
	systemContext := metadata.NewOutgoingContext(requestContext, metadata.Pairs("x-service-authorization", "Bearer dev:user"))
	_, err = settingsv1.NewUserSettingsSyncServiceClient(conn).SetUserTimezone(systemContext, &settingsv1.SetUserTimezoneRequest{UserId: "10000000-0000-4000-8000-000000000001", Timezone: "Asia/Tokyo", SettingsVersion: 2})
	if err != nil {
		t.Fatal(err)
	}
	limit, err = client.GetLimit(userContext, &assistantv1.GetLimitRequest{})
	if err != nil || limit.GetTimeZone() != "Europe/Moscow" || limit.GetRemaining() != 7 {
		t.Fatalf("profile timezone changed the Moscow daily-limit policy: %v %v", limit, err)
	}
	db, err := pgx.Connect(requestContext, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = db.Close(ctx)
	}()
	var storedZone string
	var storedVersion int32
	if err := db.QueryRow(requestContext, `SELECT time_zone, settings_version FROM user_state WHERE user_id=$1`, "10000000-0000-4000-8000-000000000001").Scan(&storedZone, &storedVersion); err != nil || storedZone != "Asia/Tokyo" || storedVersion != 2 {
		t.Fatalf("public timezone RPC was not durable: zone=%s version=%d error=%v", storedZone, storedVersion, err)
	}
	page, err := client.ListMessages(userContext, &assistantv1.ListMessagesRequest{})
	if err != nil || len(page.GetMessages()) != 0 {
		t.Fatalf("fresh history: %v %v", page, err)
	}
	keyContext := metadata.AppendToOutgoingContext(userContext, "idempotency-key", "30000000-0000-4000-8000-000000000001")
	_, err = client.AskQuestion(keyContext, &assistantv1.AskQuestionRequest{Text: "Сколько я потратил?", TransferMode: commonv1.TransferMode_TRANSFER_MODE_WITHOUT})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("unconfigured model must fail without external traffic: %v", err)
	}
	limit, err = client.GetLimit(userContext, &assistantv1.GetLimitRequest{})
	if err != nil || limit.GetRemaining() != 7 {
		t.Fatalf("unavailable generation charged a question: %v %v", limit, err)
	}
	page, err = client.ListMessages(userContext, &assistantv1.ListMessagesRequest{})
	if err != nil || len(page.GetMessages()) != 0 {
		t.Fatalf("unavailable generation created dialogue history: %v %v", page, err)
	}
	stop()
	_, err = client.GetLimit(userContext, &assistantv1.GetLimitRequest{})
	if status.Code(err) != codes.Unavailable && status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("RPC succeeded after command shutdown: %v", err)
	}
}

func commandAPIGet(ctx context.Context, client *http.Client, url string) (int, string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, "", err
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	return response.StatusCode, string(body), err
}
