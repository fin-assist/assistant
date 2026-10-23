package account

import (
	"context"
	"errors"
	"testing"
	"time"
)

type transaction struct {
	err     error
	entered bool
}

func (t *transaction) Run(ctx context.Context, fn func(context.Context) error) error {
	if t.err != nil {
		return t.err
	}
	t.entered = true
	return fn(ctx)
}

type repository struct {
	state           State
	err             error
	writes, deletes int
}

func (r *repository) State(context.Context, string) (State, error) { return r.state, r.err }
func (r *repository) SetTimezone(context.Context, string, string, int32) error {
	r.writes++
	return r.err
}
func (r *repository) Delete(context.Context, string) error { r.deletes++; return r.err }

type deleted struct {
	alive bool
	err   error
}

func (d *deleted) Guard(context.Context, string) (bool, error) { return d.alive, d.err }
func (d *deleted) Purge(ctx context.Context, id string, remove func(context.Context, string) error) error {
	if d.err != nil {
		return d.err
	}
	return remove(ctx, id)
}

func TestAccountUseCases(t *testing.T) {
	boom := errors.New("storage")
	for _, tt := range []struct {
		name                     string
		txErr, repoErr, guardErr error
		alive                    bool
		wantWrites               int
		wantErr                  error
	}{
		{name: "apply", alive: true, wantWrites: 1},
		{name: "deleted", alive: false},
		{name: "guard failure", alive: true, guardErr: boom, wantErr: boom},
		{name: "write failure", alive: true, repoErr: boom, wantWrites: 1, wantErr: boom},
		{name: "transaction failure", alive: true, txErr: boom, wantErr: boom},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tx := &transaction{err: tt.txErr}
			r := &repository{err: tt.repoErr}
			d := &deleted{alive: tt.alive, err: tt.guardErr}
			uc := New(tx, r, d, time.Now, 5)
			if err := uc.SetTimezone(t.Context(), "id", "UTC", 1); !errors.Is(err, tt.wantErr) || r.writes != tt.wantWrites {
				t.Fatalf("%v writes=%d", err, r.writes)
			}
		})
	}
	tx := &transaction{}
	r := &repository{}
	d := &deleted{alive: true}
	uc := New(tx, r, d, func() time.Time { return time.Date(2026, 10, 23, 1, 0, 0, 0, time.UTC) }, 5)
	limit, err := uc.GetLimit(t.Context(), "id")
	if err != nil || limit.Remaining != 5 {
		t.Fatalf("%+v %v", limit, err)
	}
	r.err = boom
	if _, err := uc.GetLimit(t.Context(), "id"); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if err := uc.Purge(t.Context(), "id"); !errors.Is(err, boom) || r.deletes != 1 {
		t.Fatalf("%v %d", err, r.deletes)
	}
	r.err = nil
	if err := uc.Purge(t.Context(), "id"); err != nil {
		t.Fatal(err)
	}
	d.err = boom
	if err := uc.Purge(t.Context(), "id"); !errors.Is(err, boom) {
		t.Fatal(err)
	}
}
