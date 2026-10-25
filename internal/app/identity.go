package app

import (
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"

	identitylib "github.com/fin-assist/go-identity"

	grpcclient "github.com/fin-assist/go-grpc/client"
	"github.com/fin-assist/go-grpc/server"
	"github.com/fin-assist/go-worker"

	"github.com/fin-assist/assistant/internal/config"
)

// getSigningKeys is the only user RPC identity calls; it is idempotent and retried.
const getSigningKeys = "finassist.user.v1.UserInternalService/GetSigningKeys"

// identitySetup is the identity the server and outgoing clients use, with its
// lifecycle: Job (verify only) refreshes the key caches, Close releases the user
// connection after the job has stopped.
type identitySetup struct {
	Services server.ServiceVerifier
	Users    server.UserVerifier
	Tokens   grpcclient.TokenSource
	Job      *worker.Job
	Close    func()
}

// identity builds the identity of cfg. verify delegates to go-identity
// NewAssembly (Kubernetes ServiceAccounts in prod, signed key files in stage);
// insecure-dev returns the unverified go-grpc dev verifiers, only with ENV=dev.
// dialOptions are extra options of the user connection (in-process tests only).
func identity(cfg config.Config, reg prometheus.Registerer, dialOptions []grpc.DialOption) (identitySetup, error) {
	switch cfg.Auth.Mode {
	case config.AuthInsecureDev:
		// Enforce the gate here as well: App.New is callable without config.Load.
		if cfg.Env != config.EnvDev {
			return identitySetup{}, errors.New("insecure identity requires ENV=dev")
		}
		return identitySetup{Services: server.DevServiceVerifier{}, Users: server.DevUserVerifier{},
			Tokens: grpcclient.DevToken(cfg.Service), Close: func() {}}, nil
	case config.AuthVerify:
	default:
		return identitySetup{}, errors.New("unsupported identity mode")
	}
	options := identitylib.AssemblyOptions{
		Env: string(cfg.Env), Service: cfg.Service,
		UserIssuer: cfg.Auth.Identity.UserIssuer, UserAudience: cfg.Auth.Identity.UserAudience,
		DialUser: func(tokens identitylib.TokenSource) (*grpc.ClientConn, error) {
			if cfg.Peers["user"] == "" {
				return nil, errors.New("identity: user peer required")
			}
			return grpcclient.Dial(cfg.Peers["user"], grpcclient.Options{
				Peer: "user", Tokens: tokens, DialOptions: dialOptions, Retry: []string{getSigningKeys},
			})
		},
		Clock: time.Now, Registerer: reg,
	}
	// Both sets are passed as configured: stage ignores the Kubernetes settings, and
	// the assembly refuses stage key files in prod even when New is called without
	// config.Load.
	options.Kubernetes = identitylib.KubernetesOptions{
		TokenFile: cfg.Auth.ServiceTokenFile, Issuer: cfg.Auth.Identity.KubernetesIssuer,
		JWKSURL: cfg.Auth.Identity.KubernetesJWKSURL, CAFile: cfg.Auth.Identity.KubernetesCAFile,
		APITokenFile: cfg.Auth.Identity.KubernetesAPITokenFile, Namespace: cfg.Auth.Identity.KubernetesNamespace,
	}
	options.Stage = identitylib.StageKeyFiles{
		Issuer: cfg.Auth.Identity.StageIssuer, SigningKeyFile: cfg.Auth.Identity.StageSigningKeyFile,
		PublicKeysFile: cfg.Auth.Identity.StagePublicKeysFile,
	}
	assembly, err := identitylib.NewAssembly(options)
	if err != nil {
		return identitySetup{}, err
	}
	return identitySetup{Services: assembly, Users: assembly, Tokens: assembly,
		Job:   &worker.Job{Name: "identity-keys", Run: assembly.Run},
		Close: func() { _ = assembly.Close() }}, nil
}
