package postgres

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	pgstore "github.com/fin-assist/go-postgres"

	"github.com/fin-assist/go-grpc/reqctx"
)

func TestUnitPoolConfigurationAndReadRoutingWithoutConnections(t *testing.T) {
	// pool_min_conns=0 keeps construction lazy: these checks do not execute SQL or dial.
	primary, err := pgxpool.New(t.Context(), "postgres://postgres@localhost:1/unused?sslmode=disable&pool_min_conns=0")
	if err != nil {
		t.Fatal(err)
	}
	defer primary.Close()
	replica, err := pgxpool.New(t.Context(), "postgres://postgres@localhost:1/unused?sslmode=disable&pool_min_conns=0")
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()
	db := &DB{primary: primary, replica: replica, transactions: pgstore.New(primary)}
	if db.Primary() != primary || db.Writer(t.Context()) != primary || db.Reader(t.Context()) != replica || db.Reader(reqctx.WithReadPrimary(t.Context())) != primary {
		t.Fatal("request read/write routing ignored primary hint")
	}
	if tx, ok := db.Tx(t.Context()); tx != nil || ok {
		t.Fatal("transaction exists outside service Run")
	}
}
