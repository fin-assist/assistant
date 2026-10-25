package app

import (
	"context"
	"log/slog"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/fin-assist/assistant/internal/config"
)

// verify without its configuration refuses to start instead of serving with
// identities nobody checks; dev keeps the explicit insecure verifiers.
func TestUnitIdentityFailsClosed(t *testing.T) {
	if _, err := identity(config.Config{Service: "user", Env: config.EnvProd, Auth: config.AuthConfig{Mode: config.AuthVerify}}, prometheus.NewRegistry(), nil); err == nil {
		t.Fatal("verify without configuration accepted")
	}
	id, err := identity(config.Config{Service: "user", Env: config.EnvDev, Auth: config.AuthConfig{Mode: config.AuthInsecureDev}}, prometheus.NewRegistry(), nil)
	if err != nil || id.Services == nil || id.Users == nil || id.Tokens == nil {
		t.Fatalf("dev: %v", err)
	}
	id.Close()
}

func TestIntegrationNewFailsOnUnreachableDatabase(t *testing.T) {
	cfg := config.Config{Service: "user", Env: config.EnvDev, Auth: config.AuthConfig{Mode: config.AuthInsecureDev},
		DB: config.DBConfig{PrimaryDSN: "postgres://nobody@127.0.0.1:1/db?connect_timeout=1", MaxConns: 1}}
	if _, err := New(context.Background(), cfg, slog.New(slog.DiscardHandler), prometheus.NewRegistry()); err == nil {
		t.Fatal("want an error")
	}
}
