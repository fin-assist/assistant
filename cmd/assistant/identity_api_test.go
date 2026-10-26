package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	assistantv1 "github.com/fin-assist/contracts/gen/go/finassist/assistant/v1"
	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	settingsv1 "github.com/fin-assist/contracts/gen/go/finassist/usersettings/v1"
	identitylib "github.com/fin-assist/go-identity"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/fin-assist/assistant/internal/testdb"
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
	logFile, err := os.CreateTemp(t.TempDir(), "verified-debug-log")
	if err != nil {
		t.Fatal(err)
	}
	previousStderr := os.Stderr
	os.Stderr = logFile
	defer func() { os.Stderr = previousStderr; _ = logFile.Close() }()

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
		"SERVICE_NAME": "assistant", "ENV": "prod", "AUTH_MODE": "verify", "LOG_LEVEL": "debug",
		"DB_PRIMARY_DSN": testdb.DSN(t), "DB_PRIMARY_DSN_FILE": "", "GRPC_ADDR": address, "HTTP_ADDR": "127.0.0.1:0",
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
	debugContext := metadata.AppendToOutgoingContext(callContext(serviceToken("bank-bff"), validUser),
		"x-extra-token", "PRIVATE-extra-token", "x-opaque-bin", "PRIVATE-binary", "x-public-hint", "first", "x-public-hint", "second",
		"x-request-id", "PRIVATE request id;drop")
	limit, err := client.GetLimit(debugContext, &assistantv1.GetLimitRequest{}, grpc.WaitForReady(true))
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
	stop()
	if err := logFile.Sync(); err != nil {
		t.Fatal(err)
	}
	rawLog, err := os.ReadFile(logFile.Name())
	if err != nil {
		t.Fatal(err)
	}
	logs := string(rawLog)
	// FIN-300 (go-grpc v0.2.0): an x-request-id the server refused never reaches the log.
	for _, secret := range []string{validUser, outgoing, serviceToken("bank-bff"), "api-audience-token", "PRIVATE-extra-token", "PRIVATE-binary", "PRIVATE request id"} {
		if strings.Contains(logs, secret) {
			t.Fatal("a credential or binary metadata value leaked into the actual command debug log")
		}
	}
	for _, expected := range []string{`"msg":"grpc call"`, `"authorization":"***"`, `"x-service-authorization":"***"`, `"x-extra-token":"***"`, `"x-opaque-bin":"(binary)"`, `"x-public-hint":"first,second"`, `"x-request-id":"(rejected)"`, `"request":`, `"caller":"bank-bff"`} {
		if !strings.Contains(logs, expected) {
			t.Fatalf("debug log omitted expected safe field %s", expected)
		}
	}
	if keyReads.Load() == 0 || keys.reads.Load() == 0 {
		t.Fatal("verified public requests did not consult both independent key sources")
	}
}

// commandStageKeys writes a stand's stage key files: each service's PKCS#8 PEM key
// and the shared JWKS of their public keys (kid = service name).
func commandStageKeys(t *testing.T, services ...string) (map[string]string, string, map[string]ed25519.PublicKey) {
	t.Helper()
	private, public := map[string]string{}, map[string]ed25519.PublicKey{}
	var keys []any
	for _, name := range services {
		pub, key, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		der, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		private[name] = commandAPIFile(t, name+".pem", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
		public[name] = pub
		keys = append(keys, map[string]string{"kty": "OKP", "crv": "Ed25519", "kid": name, "alg": "EdDSA", "use": "sig", "x": base64.RawURLEncoding.EncodeToString(pub)})
	}
	set, err := json.Marshal(map[string]any{"keys": keys})
	if err != nil {
		t.Fatal(err)
	}
	return private, commandAPIFile(t, "stage-jwks.json", set), public
}

// stageUserKeys plays user.GetSigningKeys on the stand: it serves user keys only
// to assistant's stage token, verified against the stand's public keys.
type stageUserKeys struct {
	userv1.UnimplementedUserInternalServiceServer
	jwks     string
	verifier *identitylib.StageVerifier
	reads    atomic.Int32
}

func (s *stageUserKeys) GetSigningKeys(ctx context.Context, _ *userv1.GetSigningKeysRequest) (*userv1.GetSigningKeysResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	service := md.Get("x-service-authorization")
	if len(service) != 1 || len(md.Get("authorization")) != 0 {
		return nil, fmt.Errorf("bootstrap credentials were not isolated from caller credentials")
	}
	if caller, err := s.verifier.VerifyService(ctx, strings.TrimPrefix(service[0], "Bearer ")); err != nil || caller != "assistant" {
		return nil, fmt.Errorf("signing keys requested without assistant's stage token")
	}
	s.reads.Add(1)
	return &userv1.GetSigningKeysResponse{JwksJson: s.jwks}, nil
}

// The real command in ENV=stage verifies stage service tokens from the key files,
// user tokens from user.GetSigningKeys, and authenticates to user with its own
// stage token; a stage without its files or user peer, and verify in dev, never start.
func TestAPICommandStageIdentityVerifiesKeyFilesAndRefusesIncompleteSetup(t *testing.T) {
	const issuer = "https://stage.test"
	private, publicKeys, public := commandStageKeys(t, "assistant", "bank-bff", "user", "analytics")
	userSide, err := identitylib.NewStageVerifier("stage", identitylib.VerifyOptions{Issuer: issuer, Audience: "finassist-internal", Clock: time.Now}, public)
	if err != nil {
		t.Fatal(err)
	}
	userPublic, userPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	peerListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	peer := grpc.NewServer()
	keys := &stageUserKeys{jwks: commandAPIJWKS(t, userPublic, "user-key"), verifier: userSide}
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
		"SERVICE_NAME": "assistant", "ENV": "stage", "AUTH_MODE": "verify", "LOG_LEVEL": "error",
		"DB_PRIMARY_DSN": testdb.DSN(t), "DB_PRIMARY_DSN_FILE": "", "GRPC_ADDR": address, "HTTP_ADDR": "127.0.0.1:0",
		"SHUTDOWN_DELAY": "0s", "SHUTDOWN_TIMEOUT": "5s", "ASSISTANT_MODEL_URI": "",
		"AUTH_USER_ISSUER": "https://user.test", "AUTH_USER_AUDIENCE": "mobile",
		"AUTH_STAGE_ISSUER": issuer, "AUTH_STAGE_SIGNING_KEY_FILE": private["assistant"], "AUTH_STAGE_PUBLIC_KEYS_FILE": publicKeys,
		"SERVICE_TOKEN_FILE": "", "PEER_USER_ADDR": "passthrough:///" + peerListener.Addr().String(),
		"PEER_BANK_STATEMENT_DATA_ADDR": "", "PEER_ANALYTICS_ADDR": "",
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
					t.Errorf("stage serve: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Error("stage serve did not stop")
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
	serviceToken := func(caller, keyOf string) string {
		source, err := identitylib.NewStageSource("stage", identitylib.StageOptions{KeyFile: private[keyOf], Caller: caller, Issuer: issuer, Clock: time.Now})
		if err != nil {
			t.Fatal(err)
		}
		token, err := source.Token(requestContext)
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	userID := "10000000-0000-4000-8000-000000000002"
	now := time.Now()
	validUser := commandAPIJWT(t, userPrivate, "user-key", map[string]any{"iss": "https://user.test", "aud": "mobile", "sub": userID, "exp": now.Add(3 * time.Minute).Unix()})
	callContext := func(service, user string) context.Context {
		return metadata.NewOutgoingContext(requestContext, metadata.Pairs("x-service-authorization", "Bearer "+service, "authorization", "Bearer "+user))
	}
	client := assistantv1.NewAssistantServiceClient(conn)
	limit, err := client.GetLimit(callContext(serviceToken("bank-bff", "bank-bff"), validUser), &assistantv1.GetLimitRequest{}, grpc.WaitForReady(true))
	if err != nil || limit.GetDailyMax() != 5 {
		t.Fatalf("stage user API failed: %v %v", limit, err)
	}
	for _, tc := range []struct {
		name, service, user string
		code                codes.Code
	}{
		{"dev-service-token", "dev:bank-bff", validUser, codes.Unauthenticated},
		{"key-of-another-service", serviceToken("bank-bff", "analytics"), validUser, codes.Unauthenticated},
		{"foreign-service", serviceToken("analytics", "analytics"), validUser, codes.PermissionDenied},
		{"user-as-service", validUser, validUser, codes.Unauthenticated},
		{"service-as-user", serviceToken("bank-bff", "bank-bff"), serviceToken("bank-bff", "bank-bff"), codes.Unauthenticated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := client.GetLimit(callContext(tc.service, tc.user), &assistantv1.GetLimitRequest{})
			if status.Code(err) != tc.code {
				t.Fatalf("stage identity boundary: status=%v want=%v", status.Code(err), tc.code)
			}
		})
	}
	systemContext := metadata.NewOutgoingContext(requestContext, metadata.Pairs("x-service-authorization", "Bearer "+serviceToken("user", "user")))
	if _, err := settingsv1.NewUserSettingsSyncServiceClient(conn).SetUserTimezone(systemContext, &settingsv1.SetUserTimezoneRequest{UserId: userID, Timezone: "UTC", SettingsVersion: 1}); err != nil {
		t.Fatalf("stage system write: %v", err)
	}
	if keys.reads.Load() == 0 {
		t.Fatal("user keys were not fetched with assistant's stage token")
	}
	stop()

	for _, tc := range []struct{ name, variable, value, reason string }{
		{"missing-signing-key", "AUTH_STAGE_SIGNING_KEY_FILE", filepath.Join(t.TempDir(), "absent.pem"), "identity:"},
		{"missing-public-keys", "AUTH_STAGE_PUBLIC_KEYS_FILE", filepath.Join(t.TempDir(), "absent.json"), "stage public keys unavailable"},
		{"unset-public-keys", "AUTH_STAGE_PUBLIC_KEYS_FILE", "", "AUTH_STAGE_PUBLIC_KEYS_FILE is required"},
		{"no-user-peer", "PEER_USER_ADDR", "", "PEER_USER_ADDR is required"},
		{"verify-in-dev", "ENV", "dev", "ENV=prod or ENV=stage"},
		{"stage-keys-in-prod", "ENV", "prod", "only with ENV=stage"},
	} {
		t.Run("refuses-"+tc.name, func(t *testing.T) {
			t.Setenv(tc.variable, tc.value)
			if tc.name == "stage-keys-in-prod" {
				t.Setenv("SERVICE_TOKEN_FILE", commandAPIFile(t, "service-token", []byte("projected")))
			}
			startContext, startCancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer startCancel()
			err := run(startContext, []string{"serve"})
			if err == nil || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("incomplete stage identity: err=%v, want %q", err, tc.reason)
			}
			if strings.Contains(err.Error(), "PRIVATE KEY") || strings.Contains(err.Error(), tc.value) && tc.value != "" && tc.variable != "ENV" {
				t.Fatalf("startup error leaked a key file path or content: %v", err)
			}
		})
	}
}
