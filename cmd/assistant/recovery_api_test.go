package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	assistantv1 "github.com/fin-assist/contracts/gen/go/finassist/assistant/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	settingsv1 "github.com/fin-assist/contracts/gen/go/finassist/usersettings/v1"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	pgstore "github.com/fin-assist/go-postgres"

	"github.com/fin-assist/assistant/internal/testdb"
)

type recoveryAPIConsent struct {
	userv1.UnimplementedUserInternalServiceServer
}

func (recoveryAPIConsent) GetAssistantConsentStatus(context.Context, *userv1.GetAssistantConsentStatusRequest) (*userv1.GetAssistantConsentStatusResponse, error) {
	version := "v1"
	return &userv1.GetAssistantConsentStatusResponse{Consent: &userv1.AssistantConsent{Granted: true, Version: &version, CurrentVersion: version}}, nil
}

func recoveryAPIResponse(w io.Writer) {
	part := map[string]any{"type": "output_text", "text": "Восстановленный помощник отвечает"}
	item := map[string]any{"id": "m", "type": "message", "role": "assistant", "status": "completed", "content": []any{part}}
	events := []map[string]any{
		{"type": "response.created", "response": map[string]any{"id": "r", "status": "in_progress", "output": []any{}}},
		{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"id": "m", "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}}},
		{"type": "response.content_part.added", "item_id": "m", "output_index": 0, "content_index": 0, "part": map[string]any{"type": "output_text", "text": ""}},
		{"type": "response.output_text.delta", "item_id": "m", "output_index": 0, "content_index": 0, "delta": part["text"]},
		{"type": "response.output_text.done", "item_id": "m", "output_index": 0, "content_index": 0, "text": part["text"]},
		{"type": "response.content_part.done", "item_id": "m", "output_index": 0, "content_index": 0, "part": part},
		{"type": "response.output_item.done", "output_index": 0, "item": item},
		{"type": "response.completed", "response": map[string]any{"id": "r", "status": "completed", "output": []any{item}, "usage": map[string]any{"input_tokens": 10, "output_tokens": 4, "total_tokens": 14, "tool_tokens": 0}}},
	}
	for _, event := range events {
		raw, _ := json.Marshal(event)
		_, _ = io.WriteString(w, "data: "+string(raw)+"\n\n")
	}
}

// The API proof follows a deployment rollback and dirty migration recovery. It
// checks persisted state and a real configured model request after correction;
// it does not claim that serve itself checks migration dirty flags.
func TestAPICommandRecoveryPreservesLibrariesAndServesConfiguredGeneration(t *testing.T) {
	dsn := testdb.DSN(t)
	peerListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	peer := grpc.NewServer()
	userv1.RegisterUserInternalServiceServer(peer, recoveryAPIConsent{})
	go func() { _ = peer.Serve(peerListener) }()
	defer peer.Stop()
	defer func() { _ = peerListener.Close() }()
	var modelCalls atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer recovery-test-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		modelCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		recoveryAPIResponse(w)
	}))
	defer model.Close()
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reserved.Close() }()
	address := reserved.Addr().String()
	for key, value := range map[string]string{
		"SERVICE_NAME": "assistant", "ENV": "dev", "AUTH_MODE": "insecure-dev", "LOG_LEVEL": "error", "DB_PRIMARY_DSN": dsn, "DB_PRIMARY_DSN_FILE": "", "GRPC_ADDR": address, "HTTP_ADDR": "127.0.0.1:0", "SHUTDOWN_DELAY": "0s", "SHUTDOWN_TIMEOUT": "5s",
		"ASSISTANT_DAILY_MAX": "5", "ASSISTANT_MODEL_URI": "gpt://folder/aliceai-llm", "ASSISTANT_MODEL_FOLDER_ID": "folder", "ASSISTANT_MODEL_ENDPOINT": model.URL + "/v1/responses", "ASSISTANT_MODEL_API_KEY_FILE": commandAPIFile(t, "model-key", []byte("recovery-test-key")),
		"PEER_USER_ADDR": "passthrough:///" + peerListener.Addr().String(), "PEER_ANALYTICS_ADDR": "passthrough:///" + peerListener.Addr().String(), "PEER_BANK_STATEMENT_DATA_ADDR": "passthrough:///" + peerListener.Addr().String(),
	} {
		t.Setenv(key, value)
	}
	previous := slog.Default()
	t.Cleanup(func() { slog.SetDefault(previous) })
	if err := run(t.Context(), []string{"migrate", "up"}); err != nil {
		t.Fatal(err)
	}
	db, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close(context.Background()) }()
	const active = "10000000-0000-4000-8000-000000000001"
	const deleted = "10000000-0000-4000-8000-000000000002"
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := db.Exec(t.Context(), sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	day := time.Now().In(time.FixedZone("Moscow", 3*3600)).Format("2006-01-02")
	exec(`INSERT INTO user_state(user_id,time_zone,settings_version,limit_day,limit_used) VALUES($1,'Asia/Tokyo',1,$2,2)`, active, day)
	exec(`INSERT INTO tombstone_users(user_id,deleted_at) VALUES($1,'2026-10-20T00:00:00Z')`, deleted)
	exec(`INSERT INTO idempotency_keys(scope,endpoint,key,request_hash,done,payload,expires_at) VALUES($1,'historical-endpoint','30000000-0000-4000-8000-000000000001',decode('abcd','hex'),true,decode('1234','hex'),now()+interval '1 day')`, active)
	checkLibraries := func(t *testing.T) {
		t.Helper()
		var tombstoneVersion, idempotencyVersion, tombstones, keys int
		err := db.QueryRow(t.Context(), `SELECT (SELECT version FROM tombstone_schema_migrations),(SELECT version FROM idempotency_schema_migrations),(SELECT count(*) FROM tombstone_users WHERE user_id=$1),(SELECT count(*) FROM idempotency_keys WHERE scope=$2 AND payload=decode('1234','hex'))`, deleted, active).Scan(&tombstoneVersion, &idempotencyVersion, &tombstones, &keys)
		if err != nil || tombstoneVersion != 1 || idempotencyVersion != 5 || tombstones != 1 || keys != 1 {
			t.Fatalf("shared library state changed: %d %d %d %d %v", tombstoneVersion, idempotencyVersion, tombstones, keys, err)
		}
	}
	checkLibraries(t)
	if err := run(t.Context(), []string{"migrate", "down", "service", "1"}); err != nil {
		t.Fatal(err)
	}
	var offTopicExists bool
	if err := db.QueryRow(t.Context(), `SELECT to_regclass('off_topic_event') IS NOT NULL`).Scan(&offTopicExists); err != nil || offTopicExists {
		t.Fatalf("service rollback did not remove off-topic table: %v %v", offTopicExists, err)
	}
	checkLibraries(t)
	exec(`UPDATE schema_migrations SET dirty=true`)
	var versions bytes.Buffer
	if err := runMigrate(t.Context(), []string{"version"}, &versions); err != nil || !strings.Contains(versions.String(), "service\t1 (dirty)") {
		t.Fatalf("dirty deployment version: %q %v", versions.String(), err)
	}
	if err := run(t.Context(), []string{"migrate", "up"}); err == nil {
		t.Fatal("dirty migration unexpectedly applied")
	}
	if err := run(t.Context(), []string{"migrate", "force", "service", "1"}); err != nil {
		t.Fatal(err)
	}
	if err := run(t.Context(), []string{"migrate", "up"}); err != nil {
		t.Fatal(err)
	}
	versions.Reset()
	if err := runMigrate(t.Context(), []string{"version"}, &versions); err != nil || strings.Contains(versions.String(), "dirty") || !strings.Contains(versions.String(), "service\t2\n") {
		t.Fatalf("corrected version: %q %v", versions.String(), err)
	}
	checkLibraries(t)
	_ = reserved.Close()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- run(ctx, []string{"serve"}) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("recovered serve: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Error("recovered serve did not stop")
			}
		})
	}
	defer stop()
	conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	requestCtx, requestCancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer requestCancel()
	user := metadata.NewOutgoingContext(requestCtx, metadata.Pairs("x-service-authorization", "Bearer dev:bank-bff", "authorization", "Bearer dev:"+active))
	client := assistantv1.NewAssistantServiceClient(conn)
	limit, err := client.GetLimit(user, &assistantv1.GetLimitRequest{}, grpc.WaitForReady(true))
	if err != nil || limit.GetRemaining() != 3 {
		t.Fatalf("recovered persisted limit: %v %v", limit, err)
	}
	system := metadata.NewOutgoingContext(requestCtx, metadata.Pairs("x-service-authorization", "Bearer dev:user"))
	if _, err := settingsv1.NewUserSettingsSyncServiceClient(conn).SetUserTimezone(system, &settingsv1.SetUserTimezoneRequest{UserId: active, Timezone: "Europe/Paris", SettingsVersion: 2}); err != nil {
		t.Fatal(err)
	}
	var zone string
	if err := db.QueryRow(requestCtx, `SELECT time_zone FROM user_state WHERE user_id=$1`, active).Scan(&zone); err != nil || zone != "Europe/Paris" {
		t.Fatalf("recovered timezone RPC was not durable: %s %v", zone, err)
	}
	revoked := metadata.NewOutgoingContext(requestCtx, metadata.Pairs("x-service-authorization", "Bearer dev:bank-bff", "authorization", "Bearer dev:"+deleted))
	if _, err := client.GetLimit(revoked, &assistantv1.GetLimitRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("rollback lost deleted-user admission: %v", err)
	}
	accepted, err := client.AskQuestion(metadata.AppendToOutgoingContext(user, "idempotency-key", "40000000-0000-4000-8000-000000000001"), &assistantv1.AskQuestionRequest{Text: "Расскажи о моих расходах", TransferMode: commonv1.TransferMode_TRANSFER_MODE_WITHOUT})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := client.WatchAnswer(user, &assistantv1.WatchAnswerRequest{AnswerId: accepted.AnswerId})
	if err != nil {
		t.Fatal(err)
	}
	for {
		event, err := stream.Recv()
		if err != nil {
			t.Fatal(err)
		}
		if event.GetError() != nil {
			t.Fatalf("recovered generation: %v", event.GetError())
		}
		if finished := event.GetDone(); finished != nil {
			if !finished.Charged || finished.RemainingLimit != 2 {
				t.Fatalf("recovered charge: %v", finished)
			}
			break
		}
	}
	page, err := client.ListMessages(user, &assistantv1.ListMessagesRequest{})
	if err != nil || len(page.GetMessages()) != 2 || len(page.Messages[1].Blocks) != 1 || page.Messages[1].Blocks[0].GetText().GetText() != "Восстановленный помощник отвечает" || page.TimeZone != "Europe/Paris" || modelCalls.Load() != 1 {
		t.Fatalf("recovered public result: %v err=%v calls=%d", page, err, modelCalls.Load())
	}

	stop()
	checkLibraries(t)
	// A healthy baseline has been observed above. A failed subsequent deploy
	// must release its listeners and must not leave the prior API serving.
	for _, failure := range []string{"missing-config-file", "database-unavailable", "database-silent", "grpc-port-in-use", "http-port-in-use", "missing-ca", "invalid-ca", "model-identity-mismatch"} {
		t.Run("restart-"+failure, func(t *testing.T) {
			var occupied net.Listener
			switch failure {
			case "missing-config-file":
				t.Setenv("DB_PRIMARY_DSN", "")
				t.Setenv("DB_PRIMARY_DSN_FILE", filepath.Join(t.TempDir(), "absent-dsn"))
			case "database-unavailable":
				t.Setenv("DB_PRIMARY_DSN", "postgres://postgres@127.0.0.1:1/postgres?sslmode=disable&connect_timeout=1")
			case "database-silent":
				// A primary that accepts and never answers: connect_timeout ends the attempt.
				t.Setenv("DB_PRIMARY_DSN", "postgres://postgres@"+silentListener(t)+"/postgres?sslmode=disable&connect_timeout=1")
			case "grpc-port-in-use", "http-port-in-use":
				target := address
				if failure == "http-port-in-use" {
					target = "127.0.0.1:0"
				}
				var err error
				occupied, err = net.Listen("tcp", target)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = occupied.Close() }()
				if failure == "http-port-in-use" {
					t.Setenv("HTTP_ADDR", occupied.Addr().String())
				}
			case "missing-ca", "invalid-ca":
				// verify exists only in prod and stage; the CA must be what fails.
				t.Setenv("ENV", "prod")
				t.Setenv("ASSISTANT_MODEL_ENDPOINT", "https://ai.api.cloud.yandex.net/v1/responses")
				t.Setenv("AUTH_MODE", "verify")
				t.Setenv("AUTH_USER_ISSUER", "https://user.test")
				t.Setenv("AUTH_USER_AUDIENCE", "mobile")
				t.Setenv("AUTH_KUBERNETES_ISSUER", "https://cluster.test")
				t.Setenv("AUTH_KUBERNETES_JWKS_URL", "https://127.0.0.1:1/keys")
				t.Setenv("AUTH_KUBERNETES_NAMESPACE", "finassist")
				t.Setenv("SERVICE_TOKEN_FILE", commandAPIFile(t, "service-token", []byte("test-service-token")))
				t.Setenv("AUTH_KUBERNETES_API_TOKEN_FILE", commandAPIFile(t, "api-token", []byte("test-api-token")))
				caFile := filepath.Join(t.TempDir(), "absent-ca.pem")
				if failure == "invalid-ca" {
					if err := os.WriteFile(caFile, []byte("not a certificate"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				t.Setenv("AUTH_KUBERNETES_CA_FILE", caFile)
			case "model-identity-mismatch":
				t.Setenv("ASSISTANT_MODEL_URI", "gpt://different-folder/aliceai-llm")
			}
			restartContext, restartCancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer restartCancel()
			err := run(restartContext, []string{"serve"})
			if err == nil {
				t.Fatal("broken deployment unexpectedly started")
			}
			// go-postgres v0.3.0 (FIN-277): an unreachable or silent primary is ErrPoolUnavailable
			// while the deploy context is live, not the deploy's own deadline.
			if (failure == "database-unavailable" || failure == "database-silent") && (!errors.Is(err, pgstore.ErrPoolUnavailable) || errors.Is(err, context.DeadlineExceeded)) {
				t.Fatalf("unavailable database reported as %v", err)
			}
			if (failure == "missing-ca" || failure == "invalid-ca") && !strings.Contains(err.Error(), "Kubernetes CA file") {
				t.Fatalf("deployment failed for another reason than the CA: %v", err)
			}
			failedConn, err := grpc.NewClient(address, grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = failedConn.Close() }()
			probeContext, probeCancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
			defer probeCancel()
			probeContext = metadata.NewOutgoingContext(probeContext, metadata.Pairs("x-service-authorization", "Bearer dev:bank-bff", "authorization", "Bearer dev:"+active))
			_, err = assistantv1.NewAssistantServiceClient(failedConn).GetLimit(probeContext, &assistantv1.GetLimitRequest{})
			if code := status.Code(err); code != codes.Unavailable && code != codes.DeadlineExceeded {
				t.Fatalf("failed deployment retained a serving API: %v", err)
			}
			checkLibraries(t)
		})
	}
}

// silentListener accepts TCP connections and never answers, like a hung primary.
func silentListener(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan net.Conn, 16)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := lis.Accept()
			if err != nil {
				return
			}
			select {
			case accepted <- c:
			default:
				_ = c.Close()
			}
		}
	}()
	t.Cleanup(func() {
		_ = lis.Close()
		<-done
		close(accepted)
		for c := range accepted {
			_ = c.Close()
		}
	})
	return lis.Addr().String()
}
