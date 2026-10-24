package app

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"

	"github.com/fin-assist/assistant/internal/config"
)

func TestUnitIdentityDevGate(t *testing.T) {
	for _, environment := range []config.Env{config.EnvProd, config.EnvStage, ""} {
		_, _, _, _, err := identity(t.Context(), config.Config{Env: environment, Auth: config.AuthConfig{Mode: config.AuthInsecureDev}}, prometheus.NewRegistry())
		if err == nil {
			t.Fatalf("insecure identity accepted in %s", environment)
		}
	}
	cfg := config.Config{Env: config.EnvDev, Service: "assistant", Auth: config.AuthConfig{Mode: config.AuthInsecureDev}}
	services, users, tokens, closeIdentity, err := identity(t.Context(), cfg, prometheus.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	defer closeIdentity()
	if got, err := services.VerifyService(t.Context(), "dev:bank-bff"); got != "bank-bff" || err != nil {
		t.Fatalf("service=%q err=%v", got, err)
	}
	if got, err := users.VerifyUser(t.Context(), "dev:user-id"); got != "user-id" || err != nil {
		t.Fatalf("user=%q err=%v", got, err)
	}
	if got, err := tokens.Token(t.Context()); got != "dev:assistant" || err != nil {
		t.Fatalf("token=%q err=%v", got, err)
	}
	cfg.Auth.Mode = "unknown"
	if _, _, _, _, err := identity(t.Context(), cfg, prometheus.NewRegistry()); err == nil {
		t.Fatal("unknown mode accepted")
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

func TestUnitIdentityTransport(t *testing.T) {
	if _, err := identityTransport(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing CA accepted")
	}
	if _, err := identityTransport(identityFile(t, "bad.pem", []byte("secret"))); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("invalid CA accepted or leaked: %v", err)
	}
}

type signingKeysServer struct {
	userv1.UnimplementedUserInternalServiceServer
	jwks string
}

func (s *signingKeysServer) GetSigningKeys(ctx context.Context, _ *userv1.GetSigningKeysRequest) (*userv1.GetSigningKeysResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	if got := md.Get("x-service-authorization"); len(got) != 1 || got[0] != "Bearer outgoing-service" {
		return nil, fmt.Errorf("missing bootstrap service credential")
	}
	if len(md.Get("authorization")) != 0 {
		return nil, fmt.Errorf("user credential forwarded")
	}
	return &userv1.GetSigningKeysResponse{JwksJson: s.jwks}, nil
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
	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	userv1.RegisterUserInternalServiceServer(grpcServer, &signingKeysServer{jwks: identityJWKS(t, userPublic, "user-key")})
	go func() { _ = grpcServer.Serve(listener) }()
	defer grpcServer.Stop()
	defer func() { _ = listener.Close() }()
	cfg := config.Config{Env: config.EnvProd, Service: "assistant", Peers: map[string]string{"user": "passthrough:///user"}, Auth: config.AuthConfig{
		Mode: config.AuthVerify, ServiceTokenFile: identityFile(t, "internal-token", []byte("outgoing-service")), Identity: config.IdentityConfig{
			UserIssuer: "https://user.example", UserAudience: "mobile", KubernetesIssuer: "https://cluster.example", KubernetesNamespace: "finassist", KubernetesJWKSURL: keysHTTP.URL,
			KubernetesAPITokenFile: identityFile(t, "api-token", []byte("api-audience-token")), KubernetesCAFile: identityFile(t, "ca.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: keysHTTP.Certificate().Raw})),
		},
	}}
	services, users, tokens, closeIdentity, err := newIdentity(t.Context(), cfg, prometheus.NewRegistry(), []grpc.DialOption{grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) })})
	if err != nil {
		t.Fatal(err)
	}
	defer closeIdentity()
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
	closeIdentity()
	closeIdentity()
}

func TestIntegrationIdentityConfigurationErrors(t *testing.T) {
	tlsServer := httptest.NewTLSServer(http.NotFoundHandler())
	defer tlsServer.Close()
	caFile := identityFile(t, "ca.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: tlsServer.Certificate().Raw}))
	valid := func() config.Config {
		return config.Config{Env: config.EnvProd, Peers: map[string]string{"user": "passthrough:///user"}, Auth: config.AuthConfig{
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
		{"API token", func(c *config.Config) { c.Auth.Identity.KubernetesAPITokenFile = "" }},
		{"CA", func(c *config.Config) { c.Auth.Identity.KubernetesCAFile = "/missing" }},
		{"JWKS URL", func(c *config.Config) { c.Auth.Identity.KubernetesJWKSURL = "http://cluster.example" }},
		{"cluster issuer", func(c *config.Config) { c.Auth.Identity.KubernetesIssuer = "" }},
		{"cluster namespace", func(c *config.Config) { c.Auth.Identity.KubernetesNamespace = "invalid:namespace" }},
		{"user peer", func(c *config.Config) { delete(c.Peers, "user") }},
		{"user issuer", func(c *config.Config) { c.Auth.Identity.UserIssuer = "" }},
		{"user audience", func(c *config.Config) { c.Auth.Identity.UserAudience = "" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := valid()
			tt.change(&cfg)
			_, _, _, closeIdentity, err := identity(t.Context(), cfg, prometheus.NewRegistry())
			if err == nil {
				closeIdentity()
				t.Fatal("invalid identity configuration accepted")
			}
		})
	}
	reg := prometheus.NewRegistry()
	cfg := valid()
	_, _, _, closeIdentity, err := identity(t.Context(), cfg, reg)
	if err != nil {
		t.Fatal(err)
	}
	defer closeIdentity()
	if _, _, _, closeSecond, err := identity(t.Context(), cfg, reg); err == nil {
		closeSecond()
		t.Fatal("metrics registration failure ignored")
	}
}
