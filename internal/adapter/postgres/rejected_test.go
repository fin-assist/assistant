package postgres

import (
	"context"
	"errors"
	"testing"

	pgstore "github.com/fin-assist/go-postgres"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fin-assist/go-grpc/reqctx"

	"github.com/fin-assist/assistant/internal/dialogue"
)

type unitBeginner struct {
	tx      pgx.Tx
	options *pgx.TxOptions
}

func (b unitBeginner) BeginTx(_ context.Context, options pgx.TxOptions) (pgx.Tx, error) {
	if b.options != nil {
		*b.options = options
	}
	return b.tx, nil
}

type unitTransaction struct {
	pgx.Tx
	commitError error
}

func (tx unitTransaction) Commit(context.Context) error { return tx.commitError }
func (unitTransaction) Rollback(context.Context) error  { return nil }

func TestUnitForeignTransactionSelectsRejectedQuerier(t *testing.T) {
	owner := pgstore.New(unitBeginner{tx: unitTransaction{}})
	foreign := &DB{transactions: pgstore.New(unitBeginner{tx: unitTransaction{}})}
	// No pools exist: choosing a pool instead of rejection must fail this test.
	err := owner.Run(context.Background(), func(ctx context.Context) error {
		for name, q := range map[string]Querier{"Writer": foreign.Writer(ctx), "Reader": foreign.Reader(ctx)} {
			if _, ok := q.(rejectedQuerier); !ok {
				t.Fatalf("%s selected %T, want rejectedQuerier", name, q)
			}
		}
		called := false
		err := NewDialogue(foreign).read(ctx, func(pgx.Tx) error { called = true; return nil })
		if err == nil || called {
			t.Errorf("foreign dialogue read: %v, callback=%v", err, called)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestUnitDialogueReadPreservesKnownErrorsAndSanitizesStorage(t *testing.T) {
	for _, kind := range []string{"success", "not-found", "storage", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			db := &DB{transactions: pgstore.New(unitBeginner{tx: unitTransaction{}})}
			ctx := t.Context()
			var failure error
			switch kind {
			case "not-found":
				failure = dialogue.ErrNotFound
			case "storage":
				failure = errors.New("private storage details")
			case "cancelled":
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
				failure = context.Canceled
			}
			err := NewDialogue(db).read(ctx, func(pgx.Tx) error { return failure })
			if kind == "storage" {
				if err == nil || err.Error() != "dialogue storage unavailable" {
					t.Fatalf("storage error was not sanitized: %v", err)
				}
			} else if !errors.Is(err, failure) {
				t.Fatalf("%s: %v, want %v", kind, err, failure)
			}
		})
	}
}

func TestUnitSharedTransactionAndReadRouting(t *testing.T) {
	primary, replica := &pgxpool.Pool{}, &pgxpool.Pool{}
	tx := &unitTransaction{}
	var selected pgx.TxOptions
	db := &DB{primary: primary, replica: replica, transactions: pgstore.New(unitBeginner{tx: tx, options: &selected})}
	ctx := t.Context()
	if _, ok := db.Tx(ctx); ok || db.Writer(ctx) != primary || db.Reader(ctx) != replica || db.Reader(reqctx.WithReadPrimary(ctx)) != primary {
		t.Fatal("routing outside a transaction changed")
	}
	options := pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}
	err := db.RunWith(ctx, options, func(ctx context.Context) error {
		actual, ok := db.Tx(ctx)
		carried, carriedOK := Tx(ctx)
		if !ok || !carriedOK || actual != tx || carried != tx || db.Reader(ctx) != tx || db.Writer(ctx) != tx {
			t.Error("transaction is not shared by all accessors")
		}
		return db.Run(ctx, func(context.Context) error { return nil })
	})
	if err != nil || selected != options {
		t.Fatalf("transaction options: %+v, %v", selected, err)
	}
}

func TestUnitRunMappedForwardsAdapterMapper(t *testing.T) {
	db := &DB{transactions: pgstore.New(unitBeginner{tx: unitTransaction{commitError: &pgconn.PgError{Code: "23503", ConstraintName: "test_fk"}}})}
	expected := errors.New("expected test refusal")
	mapper := func(err error) error {
		if pgstore.IsViolation(err, "23503", "test_fk") {
			return expected
		}
		return nil
	}
	err := db.RunMapped(t.Context(), pgx.TxOptions{}, mapper, func(ctx context.Context) error {
		called := false
		err := db.RunMapped(ctx, pgx.TxOptions{}, mapper, func(context.Context) error { called = true; return nil })
		if !errors.Is(err, pgstore.ErrNestedMapper) || called {
			t.Errorf("nested mapper: %v, callback=%v", err, called)
		}
		return nil
	})
	if !errors.Is(err, expected) {
		t.Fatalf("commit mapper was not forwarded: %v", err)
	}
}

func TestUnitRejectedQuerierAndBatchReturnForeignTransaction(t *testing.T) {
	q := rejectedQuerier{}
	ctx := context.Background()
	if _, err := q.Exec(ctx, `INSERT INTO item VALUES (1)`); !errors.Is(err, pgstore.ErrForeignTransaction) {
		t.Fatalf("Exec: %v", err)
	}
	rows, err := q.Query(ctx, `SELECT 1`)
	if rows != nil {
		rows.Close()
		t.Fatal("Query returned rows")
	}
	if !errors.Is(err, pgstore.ErrForeignTransaction) {
		t.Fatalf("Query: %v", err)
	}
	if err := q.QueryRow(ctx, `SELECT 1`).Scan(new(int)); !errors.Is(err, pgstore.ErrForeignTransaction) {
		t.Fatalf("Scan: %v", err)
	}
	batch := q.SendBatch(ctx, &pgx.Batch{})
	if _, err := batch.Exec(); !errors.Is(err, pgstore.ErrForeignTransaction) {
		t.Fatalf("batch Exec: %v", err)
	}
	rows, err = batch.Query()
	if rows != nil {
		rows.Close()
		t.Fatal("batch Query returned rows")
	}
	if !errors.Is(err, pgstore.ErrForeignTransaction) {
		t.Fatalf("batch Query: %v", err)
	}
	if err := batch.QueryRow().Scan(new(int)); !errors.Is(err, pgstore.ErrForeignTransaction) {
		t.Fatalf("batch Scan: %v", err)
	}
	if err := batch.Close(); !errors.Is(err, pgstore.ErrForeignTransaction) {
		t.Fatalf("batch Close: %v", err)
	}
}
