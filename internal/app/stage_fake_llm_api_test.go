package app_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	assistantv1 "github.com/fin-assist/contracts/gen/go/finassist/assistant/v1"
	statementv1 "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	"github.com/fin-assist/contracts/gen/go/finerr"
	identity "github.com/fin-assist/go-identity"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"

	"github.com/fin-assist/assistant/internal/app/apitest"
	"github.com/fin-assist/assistant/internal/config"
	"github.com/fin-assist/assistant/internal/fakellm"
)

type stageFakeUserPeer struct {
	*generationAPIPeers
	jwks string
}

func (p stageFakeUserPeer) GetSigningKeys(context.Context, *userv1.GetSigningKeysRequest) (*userv1.GetSigningKeysResponse, error) {
	return &userv1.GetSigningKeysResponse{JwksJson: p.jwks}, nil
}

func stageFakeFile(t *testing.T, name string, value []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, value, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
func stageFakeKey(t *testing.T, kid string) (ed25519.PrivateKey, string, map[string]string) {
	t.Helper()
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	file := stageFakeFile(t, kid+".pem", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	return key, file, map[string]string{"kty": "OKP", "crv": "Ed25519", "kid": kid, "alg": "EdDSA", "use": "sig", "x": base64.RawURLEncoding.EncodeToString(public)}
}
func stageFakeJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func stageFakeJWT(t *testing.T, key ed25519.PrivateKey, sub, issuer, audience string, expiry time.Time) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString(stageFakeJSON(t, map[string]string{"alg": "EdDSA", "typ": "JWT", "kid": "user-key"}))
	claims := base64.RawURLEncoding.EncodeToString(stageFakeJSON(t, map[string]any{"sub": sub, "iss": issuer, "aud": audience, "exp": expiry.Unix()}))
	body := header + "." + claims
	return body + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, []byte(body)))
}

func TestAPIStageFakeModelKeepsVerifiedIdentity(t *testing.T) {
	const serviceIssuer = "https://stage.example"
	const userIssuer = "https://user.example"
	const userAudience = "mobile"
	_, assistantKey, assistantPublic := stageFakeKey(t, "assistant")
	_, bffKey, bffPublic := stageFakeKey(t, "bank-bff")
	userKey, _, userPublic := stageFakeKey(t, "user-key")
	publicFile := stageFakeFile(t, "public.jwks", stageFakeJSON(t, map[string]any{"keys": []any{assistantPublic, bffPublic}}))
	public, err := base64.RawURLEncoding.DecodeString(assistantPublic["x"])
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := identity.NewStageVerifier("stage", identity.VerifyOptions{Issuer: serviceIssuer, Audience: "finassist-internal", Clock: time.Now}, map[string]ed25519.PublicKey{"assistant": ed25519.PublicKey(public)})
	if err != nil {
		t.Fatal(err)
	}
	peers := &generationAPIPeers{grant: true}
	var verifiedCalls atomic.Int32
	// Actual TCP peers use the signed stage service identity. No dev dial override
	// or user-token forwarding is needed for either financial data or signing keys.
	peerServer := grpc.NewServer(grpc.UnaryInterceptor(func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
		md, _ := metadata.FromIncomingContext(ctx)
		tokens := md.Get("x-service-authorization")
		if len(tokens) != 1 || len(tokens[0]) < 7 {
			return nil, finerr.InvalidToken()
		}
		caller, err := verifier.VerifyService(ctx, tokens[0][7:])
		if err != nil || caller != "assistant" || len(md.Get("authorization")) != 0 {
			return nil, finerr.InvalidToken()
		}
		verifiedCalls.Add(1)
		return next(ctx, req)
	}))
	userv1.RegisterUserInternalServiceServer(peerServer, stageFakeUserPeer{peers, string(stageFakeJSON(t, map[string]any{"keys": []any{userPublic}}))})
	statementv1.RegisterAssistantToolsServiceServer(peerServer, peers)
	analyticsv1.RegisterAnalyticsServiceServer(peerServer, peers)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = peerServer.Serve(listener) }()
	t.Cleanup(peerServer.Stop)
	fake, err := fakellm.Default()
	if err != nil {
		t.Fatal(err)
	}
	var providerCalls atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerCalls.Add(1)
		if r.Header.Get("Authorization") != "Bearer synthetic-stage-key" || r.Header.Get("x-data-logging-enabled") != "false" {
			t.Error("provider received wrong identity/privacy headers")
		}
		fake.ServeHTTP(w, r)
	}))
	t.Cleanup(model.Close)
	keyFile := stageFakeFile(t, "model-key", []byte("synthetic-stage-key"))
	env := apitest.Start(t, func(c *config.Config) {
		c.Service = "assistant"
		c.Env = config.EnvStage
		c.Auth = config.AuthConfig{Mode: config.AuthVerify, Identity: config.IdentityConfig{UserIssuer: userIssuer, UserAudience: userAudience, StageIssuer: serviceIssuer, StageSigningKeyFile: assistantKey, StagePublicKeysFile: publicFile}}
		for _, peer := range []string{"user", "bank-statement-data", "analytics"} {
			c.Peers[peer] = "passthrough:///" + listener.Addr().String()
		}
		c.Generation = config.GenerationConfig{ModelURI: "gpt://local/aliceai-llm-flash", FolderID: "local", Endpoint: model.URL + "/v1/responses", APIKeyFile: keyFile, InputTokensPerStep: 24000, InputTokensTotal: 48000, HistoryTokens: 16000, RequestBytesPerStep: 65536, RequestBytesTotal: 131072, MaxOutputTokens: 4096, MaxSteps: 8, MaxFunctions: 16, ParallelFunctions: 4, MaxSearchCalls: 3, FinancialResultBytes: 32768, FinancialCollectionItems: 200, ModelResponseBytes: 1048576, ModelEventBytes: 65536, ModelArgumentBytes: 16384, ModelOutputItems: 64}
	})
	source, err := identity.NewStageSource("stage", identity.StageOptions{KeyFile: bffKey, Caller: "bank-bff", Issuer: serviceIssuer, Clock: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	serviceToken, err := source.Token(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	userToken := stageFakeJWT(t, userKey, generationAPIUser, userIssuer, userAudience, time.Now().Add(time.Hour))
	rpcContext := func(service, user, key string) context.Context {
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		t.Cleanup(cancel)
		md := metadata.Pairs("x-service-authorization", "Bearer "+service, "authorization", "Bearer "+user)
		if key != "" {
			md.Set("idempotency-key", key)
		}
		return metadata.NewOutgoingContext(ctx, md)
	}
	client := assistantv1.NewAssistantServiceClient(env.Conn)
	accepted, err := client.AskQuestion(rpcContext(serviceToken, userToken, generationAPIKey()), &assistantv1.AskQuestionRequest{Text: "unknown"})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := client.WatchAnswer(rpcContext(serviceToken, userToken, ""), &assistantv1.WatchAnswerRequest{AnswerId: accepted.AnswerId})
	if err != nil {
		t.Fatal(err)
	}
	for {
		event, err := stream.Recv()
		if err != nil {
			t.Fatal(err)
		}
		if event.GetError() != nil {
			t.Fatal(event)
		}
		if event.GetDone() != nil {
			break
		}
	}
	if providerCalls.Load() == 0 || verifiedCalls.Load() == 0 {
		t.Fatal("stage generation did not reach verified peers and HTTP provider")
	}
	before := providerCalls.Load()
	foreign, _, _ := stageFakeKey(t, "foreign-user")
	for _, tc := range []struct{ name, service, user, reason string }{
		{"dev service token", "dev:bank-bff", userToken, ""},
		{"dev user token", serviceToken, "dev:" + generationAPIUser, "INVALID_TOKEN"},
		{"foreign user key", serviceToken, stageFakeJWT(t, foreign, generationAPIUser, userIssuer, userAudience, time.Now().Add(time.Hour)), "INVALID_TOKEN"},
		{"expired user token", serviceToken, stageFakeJWT(t, userKey, generationAPIUser, userIssuer, userAudience, time.Now().Add(-time.Minute)), "INVALID_TOKEN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := client.AskQuestion(rpcContext(tc.service, tc.user, generationAPIKey()), &assistantv1.AskQuestionRequest{Text: "unknown"})
			generationAPIAssertError(t, err, codes.Unauthenticated, tc.reason)
		})
	}
	other := stageFakeJWT(t, userKey, generationAPIOther, userIssuer, userAudience, time.Now().Add(time.Hour))
	watch, err := client.WatchAnswer(rpcContext(serviceToken, other, ""), &assistantv1.WatchAnswerRequest{AnswerId: accepted.AnswerId})
	if err == nil {
		_, err = watch.Recv()
	}
	generationAPIAssertError(t, err, codes.NotFound, "NOT_FOUND")
	if providerCalls.Load() != before || generationAPICount(t, env, "question") != 1 {
		t.Fatal("invalid identity started a generation")
	}
}
