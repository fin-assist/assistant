package postgres_test

import (
	"context"
	"errors"
	"testing"

	pgstore "github.com/fin-assist/go-postgres"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/fin-assist/assistant/internal/adapter/postgres"
	"github.com/fin-assist/assistant/internal/reqctx"
)

func TestIntegrationForeignTransactionCannotCrossDatabaseBoundary(t *testing.T) {
	owner, foreign := open(t), open(t)
	err := owner.Run(t.Context(), func(ctx context.Context) error {
		called := false
		err := foreign.Run(ctx, func(context.Context) error { called = true; return nil })
		if !errors.Is(err, pgstore.ErrForeignTransaction) || called {
			t.Errorf("foreign Run: %v, callback=%v", err, called)
		}
		if _, ok := foreign.Tx(ctx); ok {
			t.Error("foreign manager exposed another database's transaction")
		}
		for name, query := range map[string]postgres.Querier{"reader": foreign.Reader(ctx), "writer": foreign.Writer(ctx)} {
			_, err := query.Exec(ctx, `INSERT INTO item VALUES (42)`)
			if !errors.Is(err, pgstore.ErrForeignTransaction) {
				t.Errorf("%s foreign query: %v", name, err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if count(t, owner) != 0 || count(t, foreign) != 0 {
		t.Fatal("foreign work escaped into an owner transaction or autocommit")
	}
}

func TestIntegrationRunMappedHandlesDeferredCommitWithoutPartialWrites(t *testing.T) {
	db := open(t)
	if _, err := db.Primary().Exec(t.Context(), `CREATE TABLE parent (id integer PRIMARY KEY);
CREATE TABLE child (parent_id integer CONSTRAINT child_parent_fk REFERENCES parent(id) DEFERRABLE INITIALLY DEFERRED)`); err != nil {
		t.Fatal(err)
	}
	expected := errors.New("expected test reference refusal")
	mapper := func(err error) error {
		if pgstore.IsViolation(err, "23503", "child_parent_fk") {
			return expected
		}
		return nil
	}
	err := db.RunMapped(t.Context(), pgx.TxOptions{}, mapper, func(ctx context.Context) error {
		return db.Run(ctx, func(ctx context.Context) error {
			if err := insert(ctx, db, 1); err != nil {
				return err
			}
			_, err := db.Writer(ctx).Exec(ctx, `INSERT INTO child VALUES (42)`)
			return err
		})
	})
	var pg *pgconn.PgError
	if !errors.Is(err, expected) || errors.As(err, &pg) || count(t, db) != 0 {
		t.Fatalf("commit mapping or atomicity: %v", err)
	}
	// Unknown constraints stay internal instead of becoming a client refusal.
	err = db.RunMapped(t.Context(), pgx.TxOptions{}, mapper, func(ctx context.Context) error {
		_, err := db.Writer(ctx).Exec(ctx, `INSERT INTO item VALUES (1),(1)`)
		return err
	})
	if !pgstore.IsViolation(err, "23505", "item_pkey") || errors.Is(err, expected) || count(t, db) != 0 {
		t.Fatalf("unknown constraint was lost: %v", err)
	}
}

func TestIntegrationRunWithPreservesOptionsAndPrimaryReplicaRouting(t *testing.T) {
	primary, replica := open(t), open(t)
	if err := insert(t.Context(), primary, 1); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int{2, 3} {
		if err := insert(t.Context(), replica, id); err != nil {
			t.Fatal(err)
		}
	}
	db, err := postgres.Open(t.Context(), postgres.Options{PrimaryDSN: primary.Primary().Config().ConnString(), ReplicaDSN: replica.Primary().Config().ConnString()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	for name, ctx := range map[string]context.Context{"replica": t.Context(), "primary": reqctx.WithReadPrimary(t.Context())} {
		var rows int
		if err := db.Reader(ctx).QueryRow(ctx, `SELECT count(*) FROM item`).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		want := map[string]int{"replica": 2, "primary": 1}[name]
		if rows != want {
			t.Fatalf("%s read %d rows, want %d", name, rows, want)
		}
	}
	err = db.RunWith(t.Context(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(ctx context.Context) error {
		return db.RunWith(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(ctx context.Context) error {
			var isolation, readOnly string
			var rows int
			if err := db.Reader(ctx).QueryRow(ctx, `SELECT current_setting('transaction_isolation'),current_setting('transaction_read_only'),count(*) FROM item`).Scan(&isolation, &readOnly, &rows); err != nil {
				return err
			}
			if isolation != "repeatable read" || readOnly != "on" || rows != 1 {
				t.Errorf("outer options/primary lost: %s %s %d", isolation, readOnly, rows)
			}
			return nil
		})
	})
	if err != nil {
		t.Fatal(err)
	}
}
