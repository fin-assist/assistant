package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"log/slog"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	assistantv1 "github.com/fin-assist/contracts/gen/go/finassist/assistant/v1"
	idem "github.com/fin-assist/go-idempotency"
	"github.com/fin-assist/go-migrate"
	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"

	"github.com/fin-assist/assistant/internal/adapter/postgres"
	"github.com/fin-assist/assistant/internal/app"
	"github.com/fin-assist/assistant/internal/app/apitest"
	"github.com/fin-assist/assistant/internal/testdb"
)

// The go-idempotency v0.1.0 release this service ran before FIN-347 had only migration
// 001; v0.4.1 adds 002–005 (owner_id, resource_*, their indexes).
const idempotencyVersionOne, idempotencyVersionNow = 1, 5

func migrationConfig(dsn string) migrate.Config {
	cfg := migrate.DefaultConfig(dsn)
	cfg.Logger = slog.New(slog.DiscardHandler)
	return cfg
}

// sourcesAtIdempotencyVersionOne are the service sources with the idempotency source cut
// to its v0.1.0 content, the schema an existing assistant database has before the upgrade.
func sourcesAtIdempotencyVersionOne(t *testing.T) []migrate.Source {
	t.Helper()
	files := fstest.MapFS{}
	err := fs.WalkDir(idem.Migrations, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasPrefix(d.Name(), "001_") {
			return err
		}
		data, err := fs.ReadFile(idem.Migrations, path)
		files[path] = &fstest.MapFile{Data: data}
		return err
	})
	if err != nil || len(files) != 2 {
		t.Fatalf("idempotency 001 files: %d %v", len(files), err)
	}
	sources := app.MigrationSources()
	for i := range sources {
		if sources[i].Name == idem.MigrationsName {
			sources[i].FS = files
		}
	}
	return sources
}

func idempotencyVersion(t *testing.T, db *pgx.Conn) (version int, ownerColumn bool) {
	t.Helper()
	err := db.QueryRow(t.Context(), `SELECT (SELECT version FROM idempotency_schema_migrations),
		EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='idempotency_keys' AND column_name='owner_id')`).Scan(&version, &ownerColumn)
	if err != nil {
		t.Fatal(err)
	}
	return version, ownerColumn
}

// versionOneStored is the stored outcome exactly as go-idempotency v0.1.0 encoded it: its
// Response had no OwnerID and no Resource.
type versionOneStored struct {
	Response struct {
		Body             []byte
		Headers          map[string][]string
		DuplicateRetryAt time.Time
	}
	Status  []byte
	RetryAt time.Time
}

// FIN-347: a completed key stored by the previous release survives migrations 002–005 and
// is replayed by the upgraded engine with its original response; a different request under
// the same key is still a conflict.
func TestIntegrationIdempotencyUpgradeReplaysVersionOneKeys(t *testing.T) {
	dsn := testdb.DSN(t)
	if err := migrate.Up(t.Context(), migrationConfig(dsn), sourcesAtIdempotencyVersionOne(t)); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(t.Context())) }()
	if version, owner := idempotencyVersion(t, conn); version != idempotencyVersionOne || owner {
		t.Fatalf("starting schema: version %d, owner_id column %v", version, owner)
	}
	const user, key = "30000000-0000-4000-8000-000000000101", "30000000-0000-4000-8000-000000000102"
	endpoint := assistantv1.AssistantService_AskQuestion_FullMethodName
	hash := [32]byte{7, 7, 7}
	body, err := proto.Marshal(&assistantv1.AskQuestionResponse{AnswerId: "30000000-0000-4000-8000-000000000103", RemainingLimit: 4})
	if err != nil {
		t.Fatal(err)
	}
	var stored versionOneStored
	stored.Response.Body = body
	payload, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(t.Context(), `INSERT INTO idempotency_keys(scope,endpoint,key,request_hash,done,payload,expires_at)
		VALUES ($1,$2,$3,$4,true,$5,now()+interval '1 day')`, user, endpoint, key, hash[:], payload); err != nil {
		t.Fatal(err)
	}

	if err := migrate.Up(t.Context(), migrationConfig(dsn), app.MigrationSources()); err != nil {
		t.Fatal(err)
	}
	if version, owner := idempotencyVersion(t, conn); version != idempotencyVersionNow || !owner {
		t.Fatalf("upgraded schema: version %d, owner_id column %v", version, owner)
	}
	db, err := postgres.Open(t.Context(), postgres.Options{PrimaryDSN: dsn, AppName: "upgrade-test"})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	engine, err := idem.New(db, idem.Options{Clock: time.Now, Registerer: prometheus.NewRegistry(), Methods: map[string]idem.Policy{endpoint: {}}})
	if err != nil {
		t.Fatal(err)
	}
	reserve := func(hash [32]byte) (idem.Reservation, idem.Result, error) {
		var token idem.Reservation
		var result idem.Result
		err := db.Run(t.Context(), func(ctx context.Context) error {
			var err error
			token, result, err = engine.Reserve(ctx, idem.Request{Scope: user, Endpoint: endpoint, Key: key, Hash: hash})
			return err
		})
		return token, result, err
	}
	token, result, err := reserve(hash)
	if err != nil || !result.Replayed || token != (idem.Reservation{}) || !bytes.Equal(result.Response.Body, body) || result.Response.OwnerID != "" || result.Err != nil {
		t.Fatalf("version 1 key after upgrade: replayed=%v token=%v err=%v resultErr=%v", result.Replayed, token, err, result.Err)
	}
	if _, _, err := reserve([32]byte{8}); apitest.Reason(err) != "IDEMPOTENCY_CONFLICT" {
		t.Fatalf("another request under the upgraded key: %v", err)
	}
}

// downgradeIdempotencyToVersionOne returns the idempotency source of env to the v0.1.0
// schema with migrations 005..002 and rewrites every stored outcome in the v0.1.0 encoding,
// as if the previous release had written it.
func downgradeIdempotencyToVersionOne(t *testing.T, env *apitest.Env) {
	t.Helper()
	var source migrate.Source
	for _, s := range app.MigrationSources() {
		if s.Name == idem.MigrationsName {
			source = s
		}
	}
	if err := migrate.Down(t.Context(), migrationConfig(env.DSN), source, idempotencyVersionNow-idempotencyVersionOne); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(t.Context(), env.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(t.Context())) }()
	if version, owner := idempotencyVersion(t, conn); version != idempotencyVersionOne || owner {
		t.Fatalf("downgraded schema: version %d, owner_id column %v", version, owner)
	}
	rows, err := conn.Query(t.Context(), `SELECT scope,endpoint,key,payload FROM idempotency_keys WHERE done AND NOT encrypted`)
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		scope, endpoint, key string
		payload              []byte
	}
	var stored []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.scope, &r.endpoint, &r.key, &r.payload); err != nil {
			t.Fatal(err)
		}
		stored = append(stored, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(stored) != 2 {
		t.Fatalf("completed keys before downgrade: %d", len(stored))
	}
	for _, r := range stored {
		var current struct{ Response map[string]json.RawMessage }
		if err := json.Unmarshal(r.payload, &current); err != nil {
			t.Fatal(err)
		}
		if _, ok := current.Response["OwnerID"]; !ok {
			t.Fatal("the current engine no longer writes OwnerID: the rewrite below would test nothing")
		}
		var old versionOneStored
		if err := json.Unmarshal(r.payload, &old); err != nil {
			t.Fatal(err)
		}
		payload, err := json.Marshal(old)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(payload, []byte("OwnerID")) || bytes.Contains(payload, []byte("Resource")) {
			t.Fatalf("version 1 payload still carries new fields: %s", payload)
		}
		if _, err := conn.Exec(t.Context(), `UPDATE idempotency_keys SET payload=$4 WHERE scope=$1 AND endpoint=$2 AND key=$3`, r.scope, r.endpoint, r.key, payload); err != nil {
			t.Fatal(err)
		}
	}
}

// FIN-347 through the public API: answers accepted before the go-idempotency upgrade
// (keys in the version 1 schema and encoding) are replayed after migrations 002–005 with
// the original response, without a consent check, a second charge, a new question or
// answer, or another model call.
func TestAPIGenerationAPIReplaysKeysAcceptedBeforeIdempotencyUpgrade(t *testing.T) {
	p := &generationAPIPeers{grant: true}
	m := generationAPIModel(t, nil)
	env := generationAPIEnv(t, p, m)
	c := assistantv1.NewAssistantServiceClient(env.Conn)
	_, failed := generationAPISeedAnswer(t, env, "failed", "MODEL_ERROR", nil)
	retryKey, askKey := generationAPIKey(), generationAPIKey()
	retryReq := &assistantv1.RetryAnswerRequest{AnswerId: failed}
	retried, err := c.RetryAnswer(generationAPIContext(t, generationAPIUser, retryKey), retryReq)
	if err != nil {
		t.Fatal(err)
	}
	generationAPITerminal(t, env, generationAPIUser, retried.AnswerId)
	askReq := &assistantv1.AskQuestionRequest{Text: "Мои расходы"}
	asked, err := c.AskQuestion(generationAPIContext(t, generationAPIUser, askKey), askReq)
	if err != nil {
		t.Fatal(err)
	}
	generationAPITerminal(t, env, generationAPIUser, asked.AnswerId)
	used := func() int {
		var n int
		if err := env.App.DB.Writer(t.Context()).QueryRow(t.Context(), `SELECT limit_used FROM user_state WHERE user_id=$1`, generationAPIUser).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	charged, questions, answers, modelCalls := used(), generationAPICount(t, env, "question"), generationAPICount(t, env, "answer"), m.count.Load()

	downgradeIdempotencyToVersionOne(t, env)
	if err := migrate.Up(t.Context(), migrationConfig(env.DSN), app.MigrationSources()); err != nil {
		t.Fatal(err)
	}

	p.consent(false, false)
	consents := p.count("consent")
	againAsked, err := c.AskQuestion(generationAPIContext(t, generationAPIUser, askKey), askReq)
	if err != nil || !proto.Equal(asked, againAsked) {
		t.Fatalf("AskQuestion replay after upgrade: %v %v", againAsked, err)
	}
	againRetried, err := c.RetryAnswer(generationAPIContext(t, generationAPIUser, retryKey), retryReq)
	if err != nil || !proto.Equal(retried, againRetried) {
		t.Fatalf("RetryAnswer replay after upgrade: %v %v", againRetried, err)
	}
	if used() != charged || generationAPICount(t, env, "question") != questions || generationAPICount(t, env, "answer") != answers || m.count.Load() != modelCalls || p.count("consent") != consents {
		t.Fatalf("replay after upgrade repeated an effect: used %d/%d questions %d/%d answers %d/%d model %d/%d consent %d/%d",
			used(), charged, generationAPICount(t, env, "question"), questions, generationAPICount(t, env, "answer"), answers, m.count.Load(), modelCalls, p.count("consent"), consents)
	}
	_, err = c.AskQuestion(generationAPIContext(t, generationAPIUser, askKey), &assistantv1.AskQuestionRequest{Text: "Другой вопрос"})
	generationAPIAssertError(t, err, codes.AlreadyExists, "IDEMPOTENCY_CONFLICT")
}
