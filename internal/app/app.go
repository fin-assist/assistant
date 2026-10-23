// Package app assembles the service: identity, database pools, the gRPC server with its
// handlers, clients of other services and background jobs. cmd/assistant runs it with real
// listeners; apitest runs the same assembly in-process for API tests, so what the tests
// call is what production serves.
package app

import (
	"context"
	"log/slog"
	"time"

	tombstone "github.com/fin-assist/go-tombstone"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/fin-assist/assistant/internal/adapter/client"
	"github.com/fin-assist/assistant/internal/adapter/grpc/server"
	"github.com/fin-assist/assistant/internal/adapter/postgres"
	"github.com/fin-assist/assistant/internal/config"
	"github.com/fin-assist/assistant/internal/worker"
)

// App is the assembled service. Wire reads its fields to build adapters and usecases.
type App struct {
	Cfg        config.Config
	Log        *slog.Logger
	DB         *postgres.DB
	GRPC       *server.Server
	Clients    *client.Metrics
	Tokens     client.TokenSource
	Tombstones *tombstone.Store
	Now        func() time.Time
	Jobs       []worker.Job

	closers []func()
}

// New assembles the service from cfg. Close releases what it opened.
func New(ctx context.Context, cfg config.Config, log *slog.Logger, reg prometheus.Registerer) (*App, error) {
	services, users, tokens, closeIdentity, err := identity(ctx, cfg, reg)
	if err != nil {
		return nil, err
	}
	db, err := postgres.Open(ctx, postgres.Options{
		PrimaryDSN: cfg.DB.PrimaryDSN, ReplicaDSN: cfg.DB.ReplicaDSN,
		MaxConns: cfg.DB.MaxConns, AppName: cfg.Service,
	})
	if err != nil {
		closeIdentity()
		return nil, err
	}
	a := &App{Cfg: cfg, Log: log, DB: db, Tokens: tokens, Now: time.Now, closers: []func(){closeIdentity, db.Close}}
	fail := func(err error) (*App, error) { a.Close(); return nil, err }

	revoked, err := tombstone.NewRevokedCounter(reg)
	if err != nil {
		return fail(err)
	}
	a.Tombstones, err = tombstone.New(db, db.Primary(), a.Now)
	if err != nil {
		return fail(err)
	}
	if a.GRPC, err = server.New(server.Options{MaxRecvMsgSize: cfg.GRPC.MaxRecvMsgSize, Reflection: cfg.GRPC.Reflection}, server.Deps{
		Logger: log, Metrics: reg, Policy: AccessPolicy(),
		Services: services, Users: users, Tombstones: tombstoneAdmission{a.Tombstones, revoked},
	}); err != nil {
		return fail(err)
	}
	if a.Clients, err = client.NewMetrics(reg); err != nil {
		return fail(err)
	}
	jobs, closeWired, err := wire(a)
	if err != nil {
		return fail(err)
	}
	a.Jobs = jobs
	a.closers = append(a.closers, closeWired)
	return a, nil
}

// Close releases what New opened, in reverse order.
func (a *App) Close() {
	for i := len(a.closers) - 1; i >= 0; i-- {
		a.closers[i]()
	}
	a.closers = nil
}

// tombstoneAdmission adapts the server admission check to the shared library.
type tombstoneAdmission struct {
	store   *tombstone.Store
	revoked prometheus.Counter
}

func (a tombstoneAdmission) IsDeleted(ctx context.Context, id string) (bool, error) {
	deleted, err := a.store.Deleted(ctx, id)
	if err == nil && deleted {
		a.revoked.Inc()
	}
	return deleted, err
}
