package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"

	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	identitylib "github.com/fin-assist/go-identity"

	"github.com/fin-assist/assistant/internal/adapter/client"
	"github.com/fin-assist/assistant/internal/adapter/grpc/server"
	"github.com/fin-assist/assistant/internal/config"
)

// identity returns verifiers and the outgoing service token together with their
// lifecycle. The existing server and client interceptors retain their context and
// metadata contracts; library verifiers implement the consumer interfaces directly.
func identity(ctx context.Context, cfg config.Config, reg prometheus.Registerer) (server.ServiceVerifier, server.UserVerifier, client.TokenSource, func(), error) {
	return newIdentity(ctx, cfg, reg, nil)
}

func newIdentity(ctx context.Context, cfg config.Config, reg prometheus.Registerer, dialOptions []grpc.DialOption) (server.ServiceVerifier, server.UserVerifier, client.TokenSource, func(), error) {
	fail := func(err error) (server.ServiceVerifier, server.UserVerifier, client.TokenSource, func(), error) {
		return nil, nil, nil, nil, err
	}
	switch cfg.Auth.Mode {
	case config.AuthInsecureDev:
		// Enforce the gate here as well: App.New is callable without config.Load.
		if cfg.Env != config.EnvDev {
			return fail(errors.New("insecure identity requires ENV=dev"))
		}
		return server.DevServiceVerifier{}, server.DevUserVerifier{}, client.DevToken(cfg.Service), func() {}, nil
	case config.AuthVerify:
	default:
		return fail(errors.New("unsupported identity mode"))
	}
	if cfg.Auth.ServiceTokenFile != "" && cfg.Auth.ServiceTokenFile == cfg.Auth.Identity.KubernetesAPITokenFile {
		return fail(errors.New("identity: internal and API token files must be separate"))
	}
	tokens, err := identitylib.NewProjectedSource(cfg.Auth.ServiceTokenFile)
	if err != nil {
		return fail(errors.New("identity: service token file required"))
	}
	apiTokens, err := identitylib.NewProjectedSource(cfg.Auth.Identity.KubernetesAPITokenFile)
	if err != nil {
		return fail(errors.New("identity: Kubernetes API token file required"))
	}
	transport, err := identityTransport(cfg.Auth.Identity.KubernetesCAFile)
	if err != nil {
		return fail(err)
	}
	keys, err := identitylib.NewHTTPKeys(&http.Client{Transport: transport, Timeout: 5 * time.Second}, cfg.Auth.Identity.KubernetesJWKSURL, apiTokens)
	if err != nil {
		transport.CloseIdleConnections()
		return fail(err)
	}
	serviceKeys, err := identitylib.NewKeyCache(keys, time.Now, time.Hour)
	if err != nil {
		transport.CloseIdleConnections()
		return fail(err)
	}
	metrics, err := identitylib.NewMetrics(reg)
	if err != nil {
		transport.CloseIdleConnections()
		return fail(err)
	}
	services, err := identitylib.NewKubernetesVerifier(serviceKeys, identitylib.VerifyOptions{
		Issuer: cfg.Auth.Identity.KubernetesIssuer, Audience: "finassist-internal", Clock: time.Now, Observer: metrics,
	}, cfg.Auth.Identity.KubernetesNamespace)
	if err != nil {
		transport.CloseIdleConnections()
		return fail(err)
	}
	if cfg.Peers["user"] == "" {
		transport.CloseIdleConnections()
		return fail(errors.New("identity: user peer required"))
	}
	conn, err := client.Dial(cfg.Peers["user"], client.Options{
		Peer: "user", Tokens: tokens, DialOptions: dialOptions,
		Retry: []string{"finassist.user.v1.UserInternalService/GetSigningKeys"},
	})
	if err != nil {
		transport.CloseIdleConnections()
		return fail(errors.New("identity: user connection configuration failed"))
	}
	userKeys, err := identitylib.NewKeyCache(identitylib.UserKeys{Client: userv1.NewUserInternalServiceClient(conn)}, time.Now, 10*time.Minute)
	if err != nil {
		_ = conn.Close()
		transport.CloseIdleConnections()
		return fail(err)
	}
	users, err := identitylib.NewUserVerifier(userKeys, identitylib.VerifyOptions{
		Issuer: cfg.Auth.Identity.UserIssuer, Audience: cfg.Auth.Identity.UserAudience, Clock: time.Now, Observer: metrics,
	})
	if err != nil {
		_ = conn.Close()
		transport.CloseIdleConnections()
		return fail(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	for _, cache := range []*identitylib.KeyCache{serviceKeys, userKeys} {
		workers.Go(func() { _ = cache.Run(runCtx) })
	}
	var once sync.Once
	closeIdentity := func() {
		once.Do(func() { cancel(); workers.Wait(); _ = conn.Close(); transport.CloseIdleConnections() })
	}
	return services, users, tokens, closeIdentity, nil
}

func identityTransport(caFile string) (*http.Transport, error) {
	//nolint:gosec // The path is trusted deployment configuration, never request input.
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, errors.New("identity: cannot read Kubernetes CA file")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return nil, errors.New("identity: invalid Kubernetes CA file")
	}
	return &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}}, nil
}
