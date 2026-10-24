package postgres

import (
	"testing"

	"github.com/fin-assist/assistant/internal/reqctx"
)

func TestUnitPoolConfigurationAndReadRoutingWithoutConnections(t *testing.T) {
	// pool_min_conns=0 keeps construction lazy: these checks do not execute SQL or dial.
	primary, err := newPool(t.Context(), "postgres://postgres@localhost:1/unused?sslmode=disable&pool_min_conns=0", Options{MaxConns: 3, AppName: "primary-test"})
	if err != nil {
		t.Fatal(err)
	}
	defer primary.Close()
	replica, err := newPool(t.Context(), "postgres://postgres@localhost:1/unused?sslmode=disable&pool_min_conns=0", Options{MaxConns: 2, AppName: "replica-test"})
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()
	if cfg := primary.Config(); cfg.MaxConns != 3 || cfg.ConnConfig.RuntimeParams["application_name"] != "primary-test" {
		t.Fatalf("pool config=%v", cfg)
	}
	db := &DB{primary: primary, replica: replica}
	if db.Primary() != primary || db.Writer(t.Context()) != primary || db.Reader(t.Context()) != replica || db.Reader(reqctx.WithReadPrimary(t.Context())) != primary {
		t.Fatal("request read/write routing ignored primary hint")
	}
	if tx, ok := db.Tx(t.Context()); tx != nil || ok {
		t.Fatal("transaction exists outside service Run")
	}
}
