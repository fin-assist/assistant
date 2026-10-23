package app

import (
	"context"
	"errors"

	statementv1 "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	tombstone "github.com/fin-assist/go-tombstone"

	"github.com/fin-assist/assistant/internal/adapter/client"
	dialoguegrpc "github.com/fin-assist/assistant/internal/adapter/grpc/dialogue"
	"github.com/fin-assist/assistant/internal/dialogue"

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
	notifications, err := postgres.NewAnswerNotifications(a.context, a.DB)
	if err != nil {
		return nil, nil, err
	}
	var peer statementv1.AssistantToolsServiceClient
	closePeer := func() {}
	if target := a.Cfg.Peers["bank-statement-data"]; target != "" {
		conn, err := client.Dial(target, client.Options{Peer: "bank-statement-data", Tokens: a.Tokens, Metrics: a.Clients, Retry: []string{"finassist.statementdata.v1.AssistantToolsService/GetDataVersion"}})
		if err != nil {
			notifications.Close()
			return nil, nil, errors.New("statement data connection configuration failed")
		}
		peer = statementv1.NewAssistantToolsServiceClient(conn)
		closePeer = func() { _ = conn.Close() }
	}
	reads := dialogue.New(postgres.NewDialogue(a.DB), client.NewDataVersions(peer), notifications, allowances{uc})
	accountgrpc.Register(a.GRPC.GRPC(), uc, dialoguegrpc.New(reads))
	return nil, func() { notifications.Close(); closePeer() }, nil
}

type allowances struct{ account *account.Service }

func (a allowances) Remaining(ctx context.Context, user string) (int, error) {
	limit, err := a.account.GetLimit(ctx, user)
	return limit.Remaining, err
}
