package app

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	identitylib "github.com/fin-assist/go-identity"

	"github.com/fin-assist/assistant/internal/config"
)

func TestUnitIdentityDevGate(t *testing.T) {
	for _, environment := range []config.Env{config.EnvProd, config.EnvStage, ""} {
		if _, err := identity(config.Config{Env: environment, Auth: config.AuthConfig{Mode: config.AuthInsecureDev}}, prometheus.NewRegistry(), nil); err == nil {
			t.Fatalf("insecure identity accepted in %s", environment)
		}
	}
	cfg := config.Config{Env: config.EnvDev, Service: "assistant", Auth: config.AuthConfig{Mode: config.AuthInsecureDev}}
	id, err := identity(cfg, prometheus.NewRegistry(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer id.Close()
	if id.Job != nil {
		t.Fatal("dev identity has no keys to refresh")
	}
	if got, err := id.Services.VerifyService(t.Context(), "dev:bank-bff"); got != "bank-bff" || err != nil {
		t.Fatalf("service=%q err=%v", got, err)
	}
	if got, err := id.Users.VerifyUser(t.Context(), "dev:user-id"); got != "user-id" || err != nil {
		t.Fatalf("user=%q err=%v", got, err)
	}
	if got, err := id.Tokens.Token(t.Context()); got != "dev:assistant" || err != nil {
		t.Fatalf("token=%q err=%v", got, err)
	}
	cfg.Auth.Mode = "unknown"
	if _, err := identity(cfg, prometheus.NewRegistry(), nil); err == nil {
		t.Fatal("unknown mode accepted")
	}
}

// go-identity verifies only prod and stage: verify in dev (or an unset Env when
// App.New is called without config.Load) is refused before any peer is dialled,
// instead of falling back to a weaker mode.
func TestUnitIdentityVerifyRefusesOtherEnvironments(t *testing.T) {
	for _, environment := range []config.Env{config.EnvDev, "", "qa"} {
		dialled := false
		cfg := config.Config{Env: environment, Service: "assistant", Peers: map[string]string{"user": "passthrough:///user"}, Auth: config.AuthConfig{
			Mode: config.AuthVerify, Identity: config.IdentityConfig{UserIssuer: "https://user.example", UserAudience: "mobile"},
		}}
		id, err := identity(cfg, prometheus.NewRegistry(), []grpc.DialOption{grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			dialled = true
			return nil, errors.New("must not dial")
		})})
		if err == nil || id.Services != nil || id.Users != nil || id.Tokens != nil || id.Job != nil || dialled {
			t.Fatalf("verify accepted in %q: %v", environment, err)
		}
	}
}

func identityFile(t *testing.T, name string, content []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// signingKeysServer plays user.GetSigningKeys; it serves keys only to a caller
// presenting the assembly's outgoing service token and never a user credential.
type signingKeysServer struct {
	userv1.UnimplementedUserInternalServiceServer
	jwks   string
	accept func(context.Context, string) bool
	reads  atomic.Int32
}

func (s *signingKeysServer) GetSigningKeys(ctx context.Context, _ *userv1.GetSigningKeysRequest) (*userv1.GetSigningKeysResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	got := md.Get("x-service-authorization")
	if len(got) != 1 || !strings.HasPrefix(got[0], "Bearer ") || !s.accept(ctx, strings.TrimPrefix(got[0], "Bearer ")) {
		return nil, fmt.Errorf("missing bootstrap service credential")
	}
	if len(md.Get("authorization")) != 0 {
		return nil, fmt.Errorf("user credential forwarded")
	}
	s.reads.Add(1)
	return &userv1.GetSigningKeysResponse{JwksJson: s.jwks}, nil
}

// startSigningKeys serves keys on bufconn and returns the dial option of the user connection.
func startSigningKeys(t *testing.T, keys *signingKeysServer) []grpc.DialOption {
	t.Helper()
	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	userv1.RegisterUserInternalServiceServer(grpcServer, keys)
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(func() { grpcServer.Stop(); _ = listener.Close() })
	return []grpc.DialOption{grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) })}
}

// runIdentityJob runs the key-refresh job as the worker would and returns a stop
// function that cancels it and waits for its return.
func runIdentityJob(t *testing.T, id identitySetup) func() {
	t.Helper()
	if id.Job == nil || id.Job.Name != "identity-keys" {
		t.Fatalf("verified identity has no key-refresh job: %+v", id.Job)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- id.Job.Run(ctx) }()
	return func() {
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("identity job: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("identity job did not stop")
		}
	}
}

func identityJWT(t *testing.T, key ed25519.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	header, err := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "JWT", "kid": kid})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	data := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body)
	return data + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, []byte(data)))
}

func identityJWKS(t *testing.T, key ed25519.PublicKey, kid string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"keys": []any{map[string]string{"kty": "OKP", "crv": "Ed25519", "kid": kid, "alg": "EdDSA", "x": base64.RawURLEncoding.EncodeToString(key)}}})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestIntegrationProductionIdentity(t *testing.T) {
	userPublic, userPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	servicePublic, servicePrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	// Separate authenticated transports: user JWKS from gRPC, service JWKS from
	// the Kubernetes HTTPS endpoint with cluster CA and API-audience token.
	serviceJWKS := identityJWKS(t, servicePublic, "service-key")
	keysHTTP := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer api-audience-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(serviceJWKS))
	}))
	defer keysHTTP.Close()
	keys := &signingKeysServer{jwks: identityJWKS(t, userPublic, "user-key"), accept: func(_ context.Context, token string) bool { return token == "outgoing-service" }}
	dialOptions := startSigningKeys(t, keys)
	cfg := config.Config{Env: config.EnvProd, Service: "assistant", Peers: map[string]string{"user": "passthrough:///user"}, Auth: config.AuthConfig{
		Mode: config.AuthVerify, ServiceTokenFile: identityFile(t, "internal-token", []byte("outgoing-service")), Identity: config.IdentityConfig{
			UserIssuer: "https://user.example", UserAudience: "mobile", KubernetesIssuer: "https://cluster.example", KubernetesNamespace: "finassist", KubernetesJWKSURL: keysHTTP.URL,
			KubernetesAPITokenFile: identityFile(t, "api-token", []byte("api-audience-token")), KubernetesCAFile: identityFile(t, "ca.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: keysHTTP.Certificate().Raw})),
		},
	}}
	id, err := identity(cfg, prometheus.NewRegistry(), dialOptions)
	if err != nil {
		t.Fatal(err)
	}
	defer id.Close()
	stopJob := runIdentityJob(t, id)
	services, users, tokens := id.Services, id.Users, id.Tokens
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	now := time.Now()
	userClaims := map[string]any{"iss": "https://user.example", "aud": "mobile", "sub": "user-42", "exp": now.Add(time.Minute).Unix()}
	userToken := identityJWT(t, userPrivate, "user-key", userClaims)
	if got, err := users.VerifyUser(ctx, userToken); got != "user-42" || err != nil {
		t.Fatalf("user=%q err=%v", got, err)
	}
	serviceClaims := map[string]any{"iss": "https://cluster.example", "aud": "finassist-internal", "sub": "system:serviceaccount:finassist:bank-bff", "exp": now.Add(time.Minute).Unix(), "kubernetes.io": map[string]any{"namespace": "finassist", "serviceaccount": map[string]string{"name": "bank-bff"}}}
	serviceToken := identityJWT(t, servicePrivate, "service-key", serviceClaims)
	if got, err := services.VerifyService(ctx, serviceToken); got != "bank-bff" || err != nil {
		t.Fatalf("caller=%q err=%v", got, err)
	}
	// The key sets must never authenticate the other class of credential.
	if _, err := users.VerifyUser(ctx, serviceToken); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("service token accepted as user: %v", err)
	}
	if _, err := services.VerifyService(ctx, userToken); err == nil {
		t.Fatal("user token accepted as service")
	}
	serviceClaims["sub"] = "system:serviceaccount:finassist:analytics"
	if _, err := services.VerifyService(ctx, identityJWT(t, servicePrivate, "service-key", serviceClaims)); err == nil {
		t.Fatal("subject does not match signed serviceaccount name")
	}
	userClaims["aud"] = "another-app"
	if _, err := users.VerifyUser(ctx, identityJWT(t, userPrivate, "user-key", userClaims)); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("foreign audience accepted: %v", err)
	}
	userClaims["aud"] = "mobile"
	userClaims["exp"] = now.Add(-time.Minute).Unix()
	if _, err := users.VerifyUser(ctx, identityJWT(t, userPrivate, "user-key", userClaims)); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("expired user accepted: %v", err)
	}
	if token, err := tokens.Token(ctx); token != "outgoing-service" || err != nil {
		t.Fatalf("outgoing token not separate: %v", err)
	}
	// Replacing the projected file with an identically sized token must rotate it.
	next := identityFile(t, "next-token", []byte("rotating-service"))
	if err := os.Rename(next, cfg.Auth.ServiceTokenFile); err != nil {
		t.Fatal(err)
	}
	if token, err := tokens.Token(ctx); token != "rotating-service" || err != nil {
		t.Fatalf("outgoing token did not rotate: %v", err)
	}
	if err := os.Remove(cfg.Auth.ServiceTokenFile); err != nil {
		t.Fatal(err)
	}
	if _, err := tokens.Token(ctx); err == nil {
		t.Fatal("removed token used stale credential")
	}
	if keys.reads.Load() == 0 {
		t.Fatal("user keys were not read from user.GetSigningKeys")
	}
	stopJob()
	id.Close()
	id.Close()
}

// stageKeys writes the stage key files of a stand with the given services: each
// service's PKCS#8 PEM private key and one shared JWKS of their public keys.
func stageKeys(t *testing.T, services ...string) (map[string]string, string) {
	t.Helper()
	private := map[string]string{}
	var keys []any
	for _, name := range services {
		public, key, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		der, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		private[name] = identityFile(t, name+".pem", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
		keys = append(keys, map[string]string{"kty": "OKP", "crv": "Ed25519", "kid": name, "alg": "EdDSA", "use": "sig", "x": base64.RawURLEncoding.EncodeToString(public)})
	}
	set, err := json.Marshal(map[string]any{"keys": keys})
	if err != nil {
		t.Fatal(err)
	}
	return private, identityFile(t, "stage-jwks.json", set)
}

// stageToken signs a stage service token the way another stage service does.
func stageToken(t *testing.T, keyFile, caller, issuer string) string {
	t.Helper()
	source, err := identitylib.NewStageSource("stage", identitylib.StageOptions{KeyFile: keyFile, Caller: caller, Issuer: issuer, Clock: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	token, err := source.Token(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func TestIntegrationStageIdentity(t *testing.T) {
	const issuer = "https://stage.example"
	private, publicKeys := stageKeys(t, "assistant", "bank-bff", "user")
	userPublic, userPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	// The user service accepts only a stage token of assistant signed with its key.
	userSide, err := identitylib.NewStageVerifier("stage", identitylib.VerifyOptions{Issuer: issuer, Audience: "finassist-internal", Clock: time.Now}, stagePublicKeys(t, publicKeys))
	if err != nil {
		t.Fatal(err)
	}
	keys := &signingKeysServer{jwks: identityJWKS(t, userPublic, "user-key"), accept: func(ctx context.Context, token string) bool {
		caller, err := userSide.VerifyService(ctx, token)
		return err == nil && caller == "assistant"
	}}
	cfg := config.Config{Env: config.EnvStage, Service: "assistant", Peers: map[string]string{"user": "passthrough:///user"}, Auth: config.AuthConfig{
		Mode: config.AuthVerify, Identity: config.IdentityConfig{
			UserIssuer: "https://user.example", UserAudience: "mobile",
			StageIssuer: issuer, StageSigningKeyFile: private["assistant"], StagePublicKeysFile: publicKeys,
		},
	}}
	id, err := identity(cfg, prometheus.NewRegistry(), startSigningKeys(t, keys))
	if err != nil {
		t.Fatal(err)
	}
	defer id.Close()
	stopJob := runIdentityJob(t, id)
	defer stopJob()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if got, err := id.Services.VerifyService(ctx, stageToken(t, private["bank-bff"], "bank-bff", issuer)); got != "bank-bff" || err != nil {
		t.Fatalf("stage caller=%q err=%v", got, err)
	}
	// A key bound to one service never authenticates another name.
	if _, err := id.Services.VerifyService(ctx, stageToken(t, private["bank-bff"], "user", issuer)); err == nil {
		t.Fatal("bank-bff key accepted for user")
	}
	if _, err := id.Services.VerifyService(ctx, stageToken(t, private["bank-bff"], "bank-bff", "https://other.example")); err == nil {
		t.Fatal("foreign stage issuer accepted")
	}
	if _, err := id.Services.VerifyService(ctx, "dev:bank-bff"); err == nil {
		t.Fatal("dev token accepted in stage")
	}
	now := time.Now()
	userToken := identityJWT(t, userPrivate, "user-key", map[string]any{"iss": "https://user.example", "aud": "mobile", "sub": "user-42", "exp": now.Add(time.Minute).Unix()})
	if got, err := id.Users.VerifyUser(ctx, userToken); got != "user-42" || err != nil {
		t.Fatalf("user=%q err=%v", got, err)
	}
	if _, err := id.Services.VerifyService(ctx, userToken); err == nil {
		t.Fatal("user token accepted as stage service")
	}
	outgoing, err := id.Tokens.Token(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if caller, err := userSide.VerifyService(ctx, outgoing); caller != "assistant" || err != nil {
		t.Fatalf("outgoing stage token: caller=%q err=%v", caller, err)
	}
	if keys.reads.Load() == 0 {
		t.Fatal("user keys were not read with the stage token")
	}
}

// stagePublicKeys parses the stage JWKS as the user service would.
func stagePublicKeys(t *testing.T, file string) map[string]ed25519.PublicKey {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var set struct {
		Keys []struct{ Kid, X string } `json:"keys"`
	}
	if err := json.Unmarshal(raw, &set); err != nil {
		t.Fatal(err)
	}
	keys := map[string]ed25519.PublicKey{}
	for _, k := range set.Keys {
		x, err := base64.RawURLEncoding.DecodeString(k.X)
		if err != nil {
			t.Fatal(err)
		}
		keys[k.Kid] = ed25519.PublicKey(x)
	}
	return keys
}

func TestIntegrationIdentityConfigurationErrors(t *testing.T) {
	tlsServer := httptest.NewTLSServer(http.NotFoundHandler())
	defer tlsServer.Close()
	caFile := identityFile(t, "ca.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: tlsServer.Certificate().Raw}))
	valid := func() config.Config {
		return config.Config{Env: config.EnvProd, Service: "assistant", Peers: map[string]string{"user": "passthrough:///user"}, Auth: config.AuthConfig{
			Mode: config.AuthVerify, ServiceTokenFile: "/internal-token", Identity: config.IdentityConfig{
				UserIssuer: "https://user.example", UserAudience: "mobile", KubernetesIssuer: "https://cluster.example", KubernetesNamespace: "finassist", KubernetesJWKSURL: tlsServer.URL, KubernetesAPITokenFile: "/api-token", KubernetesCAFile: caFile,
			},
		}}
	}
	tests := []struct {
		name   string
		change func(*config.Config)
	}{
		{"service token", func(c *config.Config) { c.Auth.ServiceTokenFile = "" }},
		{"shared token file", func(c *config.Config) { c.Auth.Identity.KubernetesAPITokenFile = c.Auth.ServiceTokenFile }},
		{"default API token shared", func(c *config.Config) {
			c.Auth.Identity.KubernetesAPITokenFile = ""
			c.Auth.ServiceTokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token"
		}},
		{"CA", func(c *config.Config) { c.Auth.Identity.KubernetesCAFile = "/missing" }},
		{"JWKS URL", func(c *config.Config) { c.Auth.Identity.KubernetesJWKSURL = "http://cluster.example" }},
		{"cluster namespace", func(c *config.Config) { c.Auth.Identity.KubernetesNamespace = "invalid:namespace" }},
		{"user peer", func(c *config.Config) { delete(c.Peers, "user") }},
		{"user issuer", func(c *config.Config) { c.Auth.Identity.UserIssuer = "" }},
		{"user audience", func(c *config.Config) { c.Auth.Identity.UserAudience = "" }},
		{"stage keys in prod", func(c *config.Config) { c.Auth.Identity.StageSigningKeyFile = "/stage.pem" }},
		{"verify in dev", func(c *config.Config) { c.Env = config.EnvDev }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := valid()
			tt.change(&cfg)
			id, err := identity(cfg, prometheus.NewRegistry(), nil)
			if err == nil {
				id.Close()
				t.Fatal("invalid identity configuration accepted")
			}
		})
	}
	reg := prometheus.NewRegistry()
	cfg := valid()
	id, err := identity(cfg, reg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer id.Close()
	if second, err := identity(cfg, reg, nil); err == nil {
		second.Close()
		t.Fatal("metrics registration failure ignored")
	}
}

func TestIntegrationStageIdentityConfigurationErrors(t *testing.T) {
	private, publicKeys := stageKeys(t, "assistant")
	valid := func() config.Config {
		return config.Config{Env: config.EnvStage, Service: "assistant", Peers: map[string]string{"user": "passthrough:///user"}, Auth: config.AuthConfig{
			Mode: config.AuthVerify, Identity: config.IdentityConfig{UserIssuer: "https://user.example", UserAudience: "mobile",
				StageIssuer: "https://stage.example", StageSigningKeyFile: private["assistant"], StagePublicKeysFile: publicKeys},
		}}
	}
	if id, err := identity(valid(), prometheus.NewRegistry(), nil); err != nil {
		t.Fatal(err)
	} else {
		id.Close()
	}
	for _, tt := range []struct {
		name   string
		change func(*config.Config)
	}{
		{"issuer", func(c *config.Config) { c.Auth.Identity.StageIssuer = "" }},
		{"signing key missing", func(c *config.Config) { c.Auth.Identity.StageSigningKeyFile = "" }},
		{"signing key absent", func(c *config.Config) { c.Auth.Identity.StageSigningKeyFile = filepath.Join(t.TempDir(), "absent.pem") }},
		{"signing key invalid", func(c *config.Config) {
			c.Auth.Identity.StageSigningKeyFile = identityFile(t, "bad.pem", []byte("PRIVATE-not-a-key"))
		}},
		{"public keys missing", func(c *config.Config) { c.Auth.Identity.StagePublicKeysFile = "" }},
		{"public keys invalid", func(c *config.Config) {
			c.Auth.Identity.StagePublicKeysFile = identityFile(t, "bad.json", []byte(`{"keys":[{"kty":"RSA","kid":"x"}]}`))
		}},
		{"user peer", func(c *config.Config) { delete(c.Peers, "user") }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := valid()
			tt.change(&cfg)
			id, err := identity(cfg, prometheus.NewRegistry(), nil)
			if err == nil {
				id.Close()
				t.Fatal("invalid stage identity accepted")
			}
			if strings.Contains(err.Error(), "PRIVATE") {
				t.Fatalf("stage error leaked key file content: %v", err)
			}
			for _, path := range []string{cfg.Auth.Identity.StageSigningKeyFile, cfg.Auth.Identity.StagePublicKeysFile} {
				if path != "" && (strings.Contains(err.Error(), path) || strings.Contains(err.Error(), filepath.Base(path))) {
					t.Fatalf("stage error leaked a key file path: %v", err)
				}
			}
		})
	}
}
