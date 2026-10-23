package app

import (
	"context"
	"errors"
	"time"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	assistantv1 "github.com/fin-assist/contracts/gen/go/finassist/assistant/v1"
	statementv1 "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	idempotency "github.com/fin-assist/go-idempotency"
	tombstone "github.com/fin-assist/go-tombstone"
	"google.golang.org/grpc"

	"github.com/fin-assist/assistant/internal/adapter/aistudio"
	"github.com/fin-assist/assistant/internal/adapter/client"
	dialoguegrpc "github.com/fin-assist/assistant/internal/adapter/grpc/dialogue"
	keyadapter "github.com/fin-assist/assistant/internal/adapter/idempotency"
	"github.com/fin-assist/assistant/internal/asking"
	"github.com/fin-assist/assistant/internal/dialogue"
	"github.com/fin-assist/assistant/internal/generation"

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
		{Name: idempotency.MigrationsName, Table: idempotency.MigrationsTable, FS: idempotency.Migrations},
		{Name: "service", Table: "schema_migrations", FS: migrations.FS},
	}
}

// wire owns peer transports, the model, committed background tasks and their jobs.
func wire(a *App) (jobs []worker.Job, closeAll func(), err error) {
	repo := postgres.NewAccounts(a.DB)
	uc := account.New(a.DB, repo, a.Tombstones, a.Now, a.Cfg.DailyMax)
	notifications, err := postgres.NewAnswerNotifications(a.context, a.DB)
	if err != nil {
		return nil, nil, err
	}
	var connections []*grpc.ClientConn
	var modelHTTP *aistudio.Client
	var runner *generationRunner
	closeWired := func() {
		if runner != nil {
			runner.Close()
		}
		if modelHTTP != nil {
			modelHTTP.Close()
		}
		notifications.Close()
		for _, conn := range connections {
			_ = conn.Close()
		}
	}
	defer func() {
		if err != nil {
			closeWired()
		}
	}()
	dial := func(peer string, retry []string) (*grpc.ClientConn, error) {
		if a.Cfg.Peers[peer] == "" {
			return nil, nil
		}
		conn, e := client.Dial(a.Cfg.Peers[peer], client.Options{Peer: peer, Tokens: a.Tokens, Metrics: a.Clients, Retry: retry, DialOptions: a.peerDialOptions[peer]})
		if e != nil {
			return nil, errors.New("peer connection configuration failed")
		}
		connections = append(connections, conn)
		return conn, nil
	}
	statementConn, err := dial("bank-statement-data", []string{
		"finassist.statementdata.v1.AssistantToolsService/GetDataVersion",
		"finassist.statementdata.v1.AssistantToolsService/GetDataOverview",
		"finassist.statementdata.v1.AssistantToolsService/AggregateOperations",
		"finassist.statementdata.v1.AssistantToolsService/FindOperations",
	})
	if err != nil {
		return nil, nil, err
	}
	var statement statementv1.AssistantToolsServiceClient
	if statementConn != nil {
		statement = statementv1.NewAssistantToolsServiceClient(statementConn)
	}
	reads := dialogue.New(postgres.NewDialogue(a.DB), client.NewDataVersions(statement), notifications, allowances{uc})
	keys, err := idempotency.New(a.DB, idempotency.Options{Clock: a.Now, Registerer: a.Registry, Methods: map[string]idempotency.Policy{
		assistantv1.AssistantService_AskQuestion_FullMethodName: {},
		assistantv1.AssistantService_RetryAnswer_FullMethodName: {},
	}})
	if err != nil {
		return nil, nil, errors.New("idempotency configuration failed")
	}
	metrics, err := newGenerationMetrics(a.Registry)
	if err != nil {
		return nil, nil, errors.New("generation metrics registration failed")
	}
	var user userv1.UserInternalServiceClient
	generationRepo := postgres.NewGenerations(a.DB)
	var engine *generation.Engine
	if g := a.Cfg.Generation; g.ModelURI != "" {
		if statement == nil || a.Cfg.Peers["user"] == "" || a.Cfg.Peers["analytics"] == "" {
			return nil, nil, errors.New("generation requires user, statement data and analytics peers")
		}
		userConn, e := dial("user", []string{"finassist.user.v1.UserInternalService/GetAssistantConsentStatus"})
		if e != nil {
			return nil, nil, e
		}
		user = userv1.NewUserInternalServiceClient(userConn)
		analyticsConn, e := dial("analytics", []string{"finassist.analytics.v1.AnalyticsService/GetAnalytics", "finassist.analytics.v1.AnalyticsService/ListPeriods"})
		if e != nil {
			return nil, nil, e
		}
		data, e := client.NewFinancialData(statement, analyticsv1.NewAnalyticsServiceClient(analyticsConn), client.FinancialLimits{ResultBytes: g.FinancialResultBytes, CollectionItems: g.FinancialCollectionItems})
		if e != nil {
			return nil, nil, errors.New("financial data configuration failed")
		}
		modelHTTP, e = aistudio.New(aistudio.Options{Environment: string(a.Cfg.Env), Endpoint: g.Endpoint, FolderID: g.FolderID, Model: g.ModelURI, Key: aistudio.FileKey{Path: g.APIKeyFile},
			Limits: aistudio.Limits{RequestBytes: int64(g.RequestBytesPerStep), ResponseBytes: int64(g.ModelResponseBytes), EventBytes: g.ModelEventBytes, ArgumentBytes: g.ModelArgumentBytes, OutputItems: g.ModelOutputItems}})
		if e != nil {
			return nil, nil, e
		}
		model, e := aistudio.NewGenerationModel(modelHTTP)
		if e != nil {
			return nil, nil, e
		}
		engine, e = generation.New(generationRepo, a.DB, a.Tombstones, model, data, generation.EngineLimits{
			Budget:          generation.BudgetLimits{InputTokensPerStep: g.InputTokensPerStep, InputTokensTotal: g.InputTokensTotal, HistoryTokens: g.HistoryTokens, RequestBytesPerStep: g.RequestBytesPerStep, RequestBytesTotal: g.RequestBytesTotal, MaxSteps: g.MaxSteps, MaxFunctions: g.MaxFunctions},
			MaxOutputTokens: g.MaxOutputTokens, ParallelFunctions: g.ParallelFunctions, MaxSearchCalls: g.MaxSearchCalls, Observer: metrics,
		}, a.Now)
		if e != nil {
			return nil, nil, e
		}
	}
	// A typed nil *Engine in an interface would look configured.
	var executor generationExecutor
	if engine != nil {
		executor = engine
	}
	runner = newGenerationRunner(a.context, executor, generationRepo, a.DB, a.Tombstones, a.Now, a.Log)
	runner.metrics = metrics
	runner.notifications = notifications
	asks := asking.New(a.DB, postgres.NewQuestions(a.DB), a.Tombstones, keyadapter.New(keys), client.NewAssistantConsent(user), runner, a.Now, a.Cfg.DailyMax)
	accountgrpc.Register(a.GRPC.GRPC(), generationAccount{Service: uc, runner: runner}, dialoguegrpc.New(reads, dialoguegrpc.Options{OffTopicText: a.Cfg.OffTopic.Text}), accountgrpc.AskingOptions{UseCases: asks, Metadata: keys})
	jobs = []worker.Job{
		worker.Every("stale-generations", 5*time.Second, a.Log, runner.Reap),
		worker.Every("idempotency-cleanup", time.Minute, a.Log, func(ctx context.Context) error {
			ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			for {
				var n int64
				err := a.DB.Run(ctx, func(ctx context.Context) error { var e error; n, e = keys.Cleanup(ctx, 1000); return e })
				if err != nil {
					return err
				}
				if n < 1000 {
					return nil
				}
			}
		}),
	}
	return jobs, closeWired, nil
}

type allowances struct{ account *account.Service }

func (a allowances) Remaining(ctx context.Context, user string) (int, error) {
	limit, err := a.account.GetLimit(ctx, user)
	return limit.Remaining, err
}
