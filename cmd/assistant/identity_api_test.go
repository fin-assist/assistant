package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	assistantv1 "github.com/fin-assist/contracts/gen/go/finassist/assistant/v1"
	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	settingsv1 "github.com/fin-assist/contracts/gen/go/finassist/usersettings/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/fin-assist/assistant/internal/adapter/postgres/migrate/migratetest"
)

type commandAPIKeys struct {
	userv1.UnimplementedUserInternalServiceServer
	jwks, outgoing string
	reads          atomic.Int32
}

func (s *commandAPIKeys) GetSigningKeys(ctx context.Context, _ *userv1.GetSigningKeysRequest) (*userv1.GetSigningKeysResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	service := md.Get("x-service-authorization")
	if len(service) != 1 || service[0] != "Bearer "+s.outgoing || len(md.Get("authorization")) != 0 {
		return nil, fmt.Errorf("bootstrap credentials were not isolated from caller credentials")
	}
	s.reads.Add(1)
	return &userv1.GetSigningKeysResponse{JwksJson: s.jwks}, nil
}

func commandAPIJWT(t *testing.T, key ed25519.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	header, err := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "JWT", "kid": kid})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body)
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, []byte(unsigned)))
}

func commandAPIJWKS(t *testing.T, key ed25519.PublicKey, kid string) string {
	t.Helper()
	data, err := json.Marshal(map[string]any{"keys": []any{map[string]string{"kty": "OKP", "crv": "Ed25519", "kid": kid, "alg": "EdDSA", "x": base64.RawURLEncoding.EncodeToString(key)}}})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func commandAPIFile(t *testing.T, name string, contents []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, contents, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAPICommandVerifiedIdentitySeparatesUserServiceAndBootstrapCredentials(t *testing.T) {
	userPublic, userPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	servicePublic, servicePrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	serviceToken := func(name string) string {
		return commandAPIJWT(t, servicePrivate, "service-key", map[string]any{
			"iss": "https://cluster.test", "aud": "finassist-internal", "exp": now.Add(3 * time.Minute).Unix(),
			"sub":           "system:serviceaccount:finassist:" + name,
			"kubernetes.io": map[string]any{"namespace": "finassist", "serviceaccount": map[string]string{"name": name}},
		})
	}
	outgoing := serviceToken("assistant")
	serviceJWKS := commandAPIJWKS(t, servicePublic, "service-key")
	var keyReads atomic.Int32
	keysHTTP := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer api-audience-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		keyReads.Add(1)
		_, _ = w.Write([]byte(serviceJWKS))
	}))
	defer keysHTTP.Close()
	peerListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	peer := grpc.NewServer()
	keys := &commandAPIKeys{jwks: commandAPIJWKS(t, userPublic, "user-key"), outgoing: outgoing}
	userv1.RegisterUserInternalServiceServer(peer, keys)
	go func() { _ = peer.Serve(peerListener) }()
	defer peer.Stop()
	defer func() { _ = peerListener.Close() }()
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reserved.Close() }()
	address := reserved.Addr().String()
	for name, value := range map[string]string{
		"SERVICE_NAME": "assistant", "ENV": "prod", "AUTH_MODE": "verify", "LOG_LEVEL": "error",
		"DB_PRIMARY_DSN": migratetest.DSN(t), "DB_PRIMARY_DSN_FILE": "", "GRPC_ADDR": address, "HTTP_ADDR": "127.0.0.1:0",
		"SHUTDOWN_DELAY": "0s", "SHUTDOWN_TIMEOUT": "5s", "ASSISTANT_MODEL_URI": "",
		"AUTH_USER_ISSUER": "https://user.test", "AUTH_USER_AUDIENCE": "mobile",
		"AUTH_KUBERNETES_ISSUER": "https://cluster.test", "AUTH_KUBERNETES_JWKS_URL": keysHTTP.URL,
		"AUTH_KUBERNETES_NAMESPACE":      "finassist",
		"AUTH_KUBERNETES_CA_FILE":        commandAPIFile(t, "ca.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: keysHTTP.Certificate().Raw})),
		"AUTH_KUBERNETES_API_TOKEN_FILE": commandAPIFile(t, "api-token", []byte("api-audience-token")),
		"SERVICE_TOKEN_FILE":             commandAPIFile(t, "service-token", []byte(outgoing)),
		"PEER_USER_ADDR":                 "passthrough:///" + peerListener.Addr().String(),
		"PEER_BANK_STATEMENT_DATA_ADDR":  "", "PEER_ANALYTICS_ADDR": "",
	} {
		t.Setenv(name, value)
	}
	previousLogger := slog.Default()
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	if err := run(t.Context(), []string{"migrate", "up"}); err != nil {
		t.Fatal(err)
	}
	_ = reserved.Close()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- run(ctx, []string{"serve"}) }()
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("verified serve: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Error("verified serve did not stop")
			}
		})
	}
	defer stop()
	conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	requestContext, requestCancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer requestCancel()
	userID := "10000000-0000-4000-8000-000000000001"
	userToken := func(audience string, expiry time.Time) string {
		return commandAPIJWT(t, userPrivate, "user-key", map[string]any{"iss": "https://user.test", "aud": audience, "sub": userID, "exp": expiry.Unix()})
	}
	validUser := userToken("mobile", now.Add(3*time.Minute))
	callContext := func(service, user string) context.Context {
		return metadata.NewOutgoingContext(requestContext, metadata.Pairs("x-service-authorization", "Bearer "+service, "authorization", "Bearer "+user))
	}
	client := assistantv1.NewAssistantServiceClient(conn)
	limit, err := client.GetLimit(callContext(serviceToken("bank-bff"), validUser), &assistantv1.GetLimitRequest{}, grpc.WaitForReady(true))
	if err != nil || limit.GetDailyMax() != 5 || limit.GetRemaining() != 5 {
		t.Fatalf("verified user API failed: %v %v", limit, err)
	}
	for _, tc := range []struct {
		name, service, user string
		code                codes.Code
	}{
		{"dev-service-token", "dev:bank-bff", validUser, codes.Unauthenticated},
		{"foreign-user-audience", serviceToken("bank-bff"), userToken("other-app", now.Add(time.Minute)), codes.Unauthenticated},
		{"expired-user", serviceToken("bank-bff"), userToken("mobile", now.Add(-time.Minute)), codes.Unauthenticated},
		{"foreign-service", serviceToken("analytics"), validUser, codes.PermissionDenied},
		{"user-as-service", validUser, validUser, codes.Unauthenticated},
		{"service-as-user", serviceToken("bank-bff"), serviceToken("bank-bff"), codes.Unauthenticated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := client.GetLimit(callContext(tc.service, tc.user), &assistantv1.GetLimitRequest{})
			if status.Code(err) != tc.code {
				t.Fatalf("identity boundary: status=%v want=%v", status.Code(err), tc.code)
			}
		})
	}
	systemContext := metadata.NewOutgoingContext(requestContext, metadata.Pairs("x-service-authorization", "Bearer "+serviceToken("user")))
	_, err = settingsv1.NewUserSettingsSyncServiceClient(conn).SetUserTimezone(systemContext, &settingsv1.SetUserTimezoneRequest{UserId: userID, Timezone: "UTC", SettingsVersion: 1})
	if err != nil {
		t.Fatalf("verified system write: %v", err)
	}
	if keyReads.Load() == 0 || keys.reads.Load() == 0 {
		t.Fatal("verified public requests did not consult both independent key sources")
	}
}
