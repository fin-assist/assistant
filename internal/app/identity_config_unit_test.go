package app

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	identitylib "github.com/fin-assist/go-identity"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/fin-assist/assistant/internal/config"
)

func TestUnitIdentityRejectsInvalidConfigurationBeforeBackgroundIO(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{SerialNumber: big.NewInt(1), IsCA: true, BasicConstraintsValid: true,
		NotBefore: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), NotAfter: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, public, private)
	if err != nil {
		t.Fatal(err)
	}
	caFile := identityFile(t, "ca.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	valid := func() config.Config {
		return config.Config{Env: config.EnvProd, Service: "assistant", Peers: map[string]string{"user": "passthrough:///user"}, Auth: config.AuthConfig{
			Mode: config.AuthVerify, ServiceTokenFile: "/internal-token", Identity: config.IdentityConfig{UserIssuer: "https://user.example", UserAudience: "mobile", KubernetesIssuer: "https://cluster.example", KubernetesNamespace: "finassist", KubernetesJWKSURL: "https://cluster.invalid/jwks", KubernetesAPITokenFile: "/api-token", KubernetesCAFile: caFile},
		}}
	}
	for _, tc := range []struct {
		name   string
		change func(*config.Config)
	}{
		{"service-token", func(c *config.Config) { c.Auth.ServiceTokenFile = "" }},
		{"shared-token", func(c *config.Config) { c.Auth.Identity.KubernetesAPITokenFile = c.Auth.ServiceTokenFile }},
		// An empty API token file or issuer takes the go-identity default (config.Load
		// always supplies them); the default API token path still may not be shared.
		{"API-token-default-shared", func(c *config.Config) {
			c.Auth.Identity.KubernetesAPITokenFile = ""
			c.Auth.ServiceTokenFile = "/var/run/secrets/kubernetes.io/serviceaccount//token"
		}},
		{"missing-CA", func(c *config.Config) { c.Auth.Identity.KubernetesCAFile = "/missing" }},
		{"insecure-JWKS", func(c *config.Config) { c.Auth.Identity.KubernetesJWKSURL = "http://cluster.invalid/jwks" }},
		{"namespace", func(c *config.Config) { c.Auth.Identity.KubernetesNamespace = "invalid:namespace" }},
		{"user-peer", func(c *config.Config) { delete(c.Peers, "user") }},
		{"user-issuer", func(c *config.Config) { c.Auth.Identity.UserIssuer = "" }},
		{"user-audience", func(c *config.Config) { c.Auth.Identity.UserAudience = "" }},
		{"service-name", func(c *config.Config) { c.Service = "" }},
		{"stage-keys-in-prod", func(c *config.Config) { c.Auth.Identity.StagePublicKeysFile = "/stage-jwks.json" }},
		{"stage-without-files", func(c *config.Config) { c.Env = config.EnvStage; c.Auth.Identity.StageIssuer = "https://stage.example" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := valid()
			tc.change(&cfg)
			id, err := identity(cfg, prometheus.NewRegistry(), nil)
			if id.Close != nil {
				id.Close()
			}
			if err == nil || id.Services != nil || id.Users != nil || id.Tokens != nil || id.Job != nil || id.Close != nil {
				t.Fatalf("invalid identity config admitted: %v", err)
			}
		})
	}
	t.Run("metric-conflict", func(t *testing.T) {
		registry := prometheus.NewRegistry()
		if _, err := identitylib.NewMetrics(registry); err != nil {
			t.Fatal(err)
		}
		id, err := identity(valid(), registry, nil)
		if id.Close != nil {
			id.Close()
		}
		if err == nil {
			t.Fatal("conflicting metrics allowed background identity start")
		}
	})
}
