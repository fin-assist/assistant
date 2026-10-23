package app

import (
	tombstone "github.com/fin-assist/go-tombstone"

	"github.com/fin-assist/assistant/internal/account"
	accountgrpc "github.com/fin-assist/assistant/internal/adapter/grpc/account"
	"github.com/fin-assist/assistant/internal/adapter/grpc/server"
	"github.com/fin-assist/assistant/internal/adapter/postgres"
	"github.com/fin-assist/assistant/internal/adapter/postgres/migrate"
	"github.com/fin-assist/assistant/internal/worker"
	"github.com/fin-assist/assistant/migrations"
)

// AccessPolicy follows grpc-api.md 3.3, including the dialogue RPCs implemented later.
func AccessPolicy() server.Policy {
	return server.Policy{
		"/finassist.assistant.v1.AssistantService/*":           {Callers: []string{"bank-bff"}, Model: server.ModelUser},
		"/finassist.usersettings.v1.UserSettingsSyncService/*": {Callers: []string{"user"}, Model: server.ModelSystem},
		"/finassist.userdeletion.v1.UserDeletionService/*":     {Callers: []string{"user"}, Model: server.ModelSystem},
	}
}

// MigrationSources keeps each library's migration history separate from the service.
func MigrationSources() []migrate.Source {
	return []migrate.Source{
		{Name: tombstone.MigrationsName, Table: tombstone.MigrationsTable, FS: tombstone.Migrations},
		{Name: "service", Table: "schema_migrations", FS: migrations.FS},
	}
}

// wire registers the account handlers. Dialogue generation is a later FIN-220 stage.
func wire(a *App) (jobs []worker.Job, closeAll func(), err error) {
	repo := postgres.NewAccounts(a.DB)
	uc := account.New(a.DB, repo, a.Tombstones, a.Now, a.Cfg.DailyMax)
	accountgrpc.Register(a.GRPC.GRPC(), uc)
	return nil, func() {}, nil
}
