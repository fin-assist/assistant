package postgres

import (
	"context"
	"errors"
	"reflect"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

var errUnitDriver = errors.New("PRIVATE driver details")

type unitRow struct {
	values []any
	err    error
}

func (r unitRow) Scan(destinations ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(destinations) != len(r.values) {
		return errors.New("unit row column mismatch")
	}
	for i, destination := range destinations {
		target := reflect.ValueOf(destination).Elem()
		if r.values[i] == nil {
			target.SetZero()
			continue
		}
		value := reflect.ValueOf(r.values[i])
		if !value.Type().AssignableTo(target.Type()) {
			return errors.New("unit row type mismatch")
		}
		target.Set(value)
	}
	return nil
}
func row(values ...any) unitRow { return unitRow{values: values} }

type unitRows struct {
	values [][]any
	cursor int
	err    error
}

func (r *unitRows) Close()                                       {}
func (r *unitRows) Err() error                                   { return r.err }
func (r *unitRows) CommandTag() pgconn.CommandTag                { return pgconn.NewCommandTag("SELECT 0") }
func (r *unitRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *unitRows) Next() bool {
	if r.cursor >= len(r.values) {
		return false
	}
	r.cursor++
	return true
}
func (r *unitRows) Scan(destinations ...any) error {
	return row(r.values[r.cursor-1]...).Scan(destinations...)
}
func (r *unitRows) Values() ([]any, error) { return r.values[r.cursor-1], nil }
func (r *unitRows) RawValues() [][]byte    { return nil }
func (r *unitRows) Conn() *pgx.Conn        { return nil }

// unitTx is a declared pgx.Tx port fake. No SQL semantics are claimed here;
// integration tests exercise the same repository paths on real PostgreSQL.
type unitTx struct {
	commands                              []string
	rows                                  []unitRow
	queryRows                             []*unitRows
	rowIndex, queryIndex, execs, failExec int
	affected                              int64
}

func (tx *unitTx) context(ctx context.Context) context.Context {
	return context.WithValue(ctx, txKey{}, pgx.Tx(tx))
}
func (tx *unitTx) QueryRow(context.Context, string, ...any) pgx.Row {
	if tx.rowIndex >= len(tx.rows) {
		return unitRow{err: errUnitDriver}
	}
	result := tx.rows[tx.rowIndex]
	tx.rowIndex++
	return result
}
func (tx *unitTx) Query(context.Context, string, ...any) (pgx.Rows, error) {
	if tx.queryIndex >= len(tx.queryRows) {
		return nil, errUnitDriver
	}
	result := tx.queryRows[tx.queryIndex]
	tx.queryIndex++
	return result, nil
}
func (tx *unitTx) Exec(_ context.Context, command string, _ ...any) (pgconn.CommandTag, error) {
	tx.commands = append(tx.commands, command)
	tx.execs++
	if tx.failExec == tx.execs {
		return pgconn.CommandTag{}, errUnitDriver
	}
	if tx.affected == 0 {
		return pgconn.NewCommandTag("UPDATE 0"), nil
	}
	return pgconn.NewCommandTag("UPDATE 1"), nil
}
func (tx *unitTx) Begin(context.Context) (pgx.Tx, error) { return nil, errUnitDriver }
func (tx *unitTx) Commit(context.Context) error          { return errUnitDriver }
func (tx *unitTx) Rollback(context.Context) error        { return errUnitDriver }
func (tx *unitTx) CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error) {
	return 0, errUnitDriver
}
func (tx *unitTx) SendBatch(context.Context, *pgx.Batch) pgx.BatchResults { return nil }
func (tx *unitTx) LargeObjects() pgx.LargeObjects                         { return pgx.LargeObjects{} }
func (tx *unitTx) Prepare(context.Context, string, string) (*pgconn.StatementDescription, error) {
	return nil, errUnitDriver
}
func (tx *unitTx) Conn() *pgx.Conn { return nil }

func (r *unitRows) TypeMap() *pgtype.Map { return pgtype.NewMap() }
