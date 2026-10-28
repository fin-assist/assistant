# Instructions for code agents

FIN-368 rollout requires revision-capable bank-statement-data and analytics with complete acknowledged vectors for consumed snapshots before BFF/assistant. Legacy snapshots stay unknown until recalculation; empty vectors cannot certify a monthly chart. BFF must understand freshness codes before assistant emits them. Current-version evidence ignores obsolete wall-clock fields.

The common catalog Block.id is optional: actual analytics sends an anonymous heading.
FinancialData accepts it without synthesizing IDs while retaining primitive/state/lock/number guards.

The `assistant` service of «Понятные финансы» (B2C personal finance: OFX statement import from T-Bank, operations,
analytics, LLM assistant): questions to Alice AI with function calling, the answer stream, dialogue history, the daily
question limit and the user's time zone (contracts: `docs/api.md` section 7, `docs/grpc-api.md` 7.4). Made from
`fin-assist/go-service-template`. Read by Claude Code (through `CLAUDE.md`) and by Codex. This file is the
orientation map: read it and `README.md` first, then open only what the task touches.

## Keep this file current

A PR that changes what this file, `README.md` or `docs/` say (layout, commands, CI checks, conventions, process)
updates them in the same PR. A service made from the template rewrites the first paragraph for itself.
- Blocks between `<!-- generated:… -->` markers are produced from code; do not edit them by hand. The tables of
  `docs/config.md` come from `Vars` in `internal/config/vars.go`: a new variable or default is a line there, then
  `go test ./internal/config -run TestUnitDocIsCurrent -update`. `go test` fails on a stale table, on a variable read
  but not declared, and on one declared but never read.

## Where things are decided

- Contracts: `fin-assist/contracts` — `docs/api.md` (REST), `docs/grpc-api.md` (services, call models, metadata,
  errors, idempotency, outbox, transport), `docs/calc.md` (calculations), `proto/`. The service imports the generated
  module `github.com/fin-assist/contracts/gen/go` (messages, gRPC, `finerr`) at a tag `gen/go/vX.Y.Z`. A service
  never defines its own `.proto`, error codes or REST fields: a change goes to `contracts` first (its own FIN issue
  and PR), then the service bumps the module.
- Shared code: Go libraries made from `fin-assist/go-library-template` (FIN-118) — `go-identity`, `go-idempotency`,
  `go-outbox`, `go-tombstone`, `go-postgres`, `go-grpc`, `go-migrate`, `go-worker`. The service imports them at tagged versions and wires them in `internal/app` by the
  «Внедрение» section of their README; versions and compatibility — `docs/releasing.md` of the library template.
  Code that more than one service needs goes into a library (its own FIN issue), not into a copy here.
- Architecture, MVP scope, product decisions: Confluence PF (https://finassist.atlassian.net/wiki/spaces/PF),
  «Архитектура» (including «Миграции и доступ к БД» and «Репозитории и структура кода»). When the docs here and the
  contracts differ, the contracts and the owner's latest decision win; say so in the PR.
- Every change goes through a YouTrack issue (`FIN-<n>`, finassist.youtrack.cloud) referenced in the PR.
- Mismatches between the contracts, Confluence and this repository are expected. Resolve them in favour of the
  contracts and the owner's latest decision, say so in the PR, and propose (not impose) engineering changes; keep them
  non-radical. The owner writes backend code himself and is strong in PostgreSQL: schemas and queries are proposed
  with arguments, not decided for him.

## Repository map

| What | Where |
|---|---|
| Commands `serve`, `migrate`: listeners, shutdown | `cmd/assistant/` (`serve.go`, `migrate.go`) |
| Assembly: identity, pools, server; wiring of adapters, usecases and handlers (`wire()`), the access table (`AccessPolicy()`), migration sources | `internal/app/` (`app.go`, `wire.go`) |
| The whole service in-process for API tests | `internal/app/apitest/` |
| Entities and invariants (no pgx, gRPC or generated code) | `internal/domain/` |
| Usecases and the interfaces of repositories and clients they need | `internal/<feature>/` |
| gRPC server, interceptors, access table types, identity and tombstone interfaces | `github.com/fin-assist/go-grpc/server`; rules — `docs/server.md` |
| gRPC handlers (messages ↔ usecases) | `internal/adapter/grpc/<feature>/` |
| Pools, transaction manager, repositories | `internal/adapter/postgres/`; rules — `docs/migrations.md`, «Запросы на pgx» |
| Migration runner and the PostgreSQL for tests | `github.com/fin-assist/go-migrate`, `go-migrate/migratetest`, `internal/testdb/` |
| SQL migrations (embedded) | `migrations/`; rules — `docs/migrations.md` |
| Database schema: entities, keys, rules kept by the code, the queries and indexes they serve | `docs/schema.md`; keys and queries tested in `migrations/schema_test.go` |
| Question acceptance/retry, guarded keys and post-commit launch | `internal/asking/`, `internal/adapter/idempotency/`, `adapter/grpc/account/asking.go`, `adapter/postgres/asking.go` |
| Pure OFF_TOPIC policy and terminal effects; atomic ledger application and post-commit metrics | `internal/domain/`, `internal/asking/`, `generation/ports.go`, `adapter/postgres/off_topic.go`; policy — `docs/generation.md` (FIN-161, FIN-232) |
| Phase-owned original-RPC retry permission; canonical terminal/history cause | `asking.RequestRetryError`, `adapter/idempotency.ContractError`; `adapter/grpc/dialogue/mapper.go` dual-writes `finerr.NewResultStatus` and legacy fields using contracts gen/go v0.6.0; rollout — `docs/server.md` (FIN-225) |
| Validated tool registry, typed handlers and metric names | `internal/generation/tool_registry.go`, `builtin_tools.go`; assembled once in `internal/app/wire.go` |
| Stateless model/tool cycle, budgets and terminal accounting | `internal/generation/`, `adapter/postgres/generation.go`; policy and remaining work — `docs/generation.md` |
| Server-built chart/rows, trusted navigation and selected LOCKED; provider citation list | `internal/generation/presenter.go`, `citations.go`; `present_result` schema in `builtin_tools.go` |
| Exact foreign-operation text amounts and ISO 4217 decimal metadata | `internal/generation/currency.go`; never divide every currency by 100 or format money through float64 |
| Loopback Responses scenario handler, binary, dev compose overlay and optional stage systemd unit/drop-in; synthetic provider recordings | `internal/fakellm/`, `cmd/fake-llm/`, `compose.fake-llm.yaml`, `docs/fake-llm.md`; FIN-142 |
| AI Studio Responses transport and model port adapter (canonical function correlation uses unique item ID, never the non-unique provider call_id) | `internal/adapter/aistudio/`; protocol/security/HTTP tests — `docs/aistudio.md` |
| Task lifecycle, cross-instance deletion cancellation, expiry/key jobs and metrics | `internal/app/generation_runner.go`, `generation_metrics.go`, `wire.go` |
| Service financial-data adapters; common transport/token source | `internal/adapter/client/`; `github.com/fin-assist/go-grpc/client` |
| Financial-data/consent adapters and transport-independent tool facts | `internal/adapter/client/financial_data.go`, `financial_facts.go`, `consent.go`, `internal/tooldata/`; status — `docs/tools.md` |
| Background jobs | `github.com/fin-assist/go-worker` |
| Configuration: every variable with its default and description | `internal/config/vars.go` (`Vars`); generated tables — `docs/config.md` |
| Request-scoped values (request ID, caller, user, bank-bff hints) | `github.com/fin-assist/go-grpc/reqctx` |
| Logger, metrics endpoint, readiness | `internal/logx/`, `internal/ops/` |
| Image, local run, stand | `Dockerfile`, `compose.yaml`, `Makefile`, `deploy/systemd/` |
| CI, lint | `.github/workflows/ci.yml`, `.golangci.yml`, `.squawk.toml` |
| Service versions, release notes and repeatable GitHub Release | `CHANGELOG.md`, `docs/releasing.md`, `.github/workflows/release.yml`, `scripts/changelog.py` |
| Dependency updates | `renovate.json` |
| PR review by Claude | `.github/workflows/claude.yml` |

Shared infrastructure (FIN-256): go-grpc v0.2.0, go-worker v0.1.1, go-migrate v0.2.0, go-postgres v0.3.0 and go-idempotency v0.6.0 are tagged dependencies (FIN-347). One shared reqctx is used by handlers, logging, clients and read routing; business adapters and migration-source order remain service-owned. The public go-migrate/migratetest harness owns database isolation and up/down/up checks; internal/testdb only selects the test server.

### Implemented account foundation (FIN-220)

`internal/account/` owns daily-limit calculation and versioned settings/deletion use cases;
`internal/adapter/postgres/account.go` stores them; `internal/adapter/grpc/account/` registers GetLimit,
SetUserTimezone and PurgeUserData. `internal/dialogue/` owns history and durable-answer observation, with adapters in `grpc/dialogue/` and
`postgres/dialogue.go`. AskQuestion and RetryAnswer use go-idempotency v0.6.0, primary consent, one atomic
charge/create/key transaction and post-commit generation. A model URI and all three peers enable the model/tool cycle;
without the model new calls return UNAVAILABLE without charge. Primary snapshots are repeatable-read; one application-owned notification connection listens
before subscribers read their first snapshot. Writers notify `assistant_answers` with answer UUID in the same
transaction; deletion notifies `assistant_users` to wake streams and cancel that user's generators on every instance. Sources use the statement-data GetDataVersion
system RPC; a source-bearing read requires the configured peer. FIN-368 stores exact revision sets from all five financial readers (including stale analytics and period navigation) in answer.data_revisions; compare against current primary revisions, never wall timestamps. Missing/legacy proof is unverified; timezone differences have their own code. Persisted JSON null must never become known zero. Only bare version-peer UNAVAILABLE/DEADLINE_EXCEEDED with a live parent degrades to unverified; auth/permission, malformed vectors and parent cancellation fail the whole read. Watch refreshes primary proof on every source-bearing durable update. Financial adapters normalize typed rendering facts separately from the
model JSON; the presenter never decodes a peer protobuf envelope or enum name. Never forward user tokens.
`internal/app/identity.go` maps configuration to go-identity v0.2.0 `NewAssembly` (FIN-324): prod verifies Kubernetes
ServiceAccounts, stage verifies signed key files (`AUTH_STAGE_*`), both verify user JWT from `GetSigningKeys`;
verify is refused in dev, insecure-dev only in dev. FIN-149 permits the model HTTP endpoint on a numeric loopback IP in stage; it does not enable dev authentication or peer dial overrides. Production remains the exact official HTTPS endpoint. The assembly's `Run` is the `identity-keys` worker job; App.Close
releases it after the jobs stop. go-tombstone v0.1.0 is wired in app.go; tombstone and idempotency migration sources
precede the service. Every new user write must call Tombstones.Guard inside the service transaction before row locks;
late system events acknowledge deleted users without effects. Account reads use primary. IANA tzdata is embedded.

### Stack

Versions are in `go.mod` (Go toolchain included), `.github/workflows/ci.yml` and `Makefile` (golangci-lint), `compose.yaml`
and `testdb.DefaultImage` (PostgreSQL). Main pieces: grpc-go, protobuf, protovalidate, pgx v5, golang-migrate,
prometheus client_golang, testcontainers-go, squawk; the fin-assist libraries above. A new dependency is named in
the PR with why it is needed.

## Build in an agent container

- Go: the toolchain from `go.mod` is downloaded by `GOTOOLCHAIN=auto` (the default) on the first `go` command.
- `fin-assist/*` modules come from git, not the proxy: `GOPRIVATE=github.com/fin-assist/*` (set by `Makefile`; export
  it for plain `go` commands).
- Docker is usually absent in an agent container, so testcontainers cannot start PostgreSQL. Run a local server and
  point the tests at it: `TEST_DATABASE_URL='postgres://postgres@localhost:<port>/postgres?sslmode=disable' make test`.
  Without either, integration tests are skipped locally and fail in CI (`CI` set) — a skip is not a pass.
- `make lint` runs the pinned golangci-lint through `go run`; no separate install.
- `make cover` (or `go test -coverpkg=./... -coverprofile=cover.out ./...`) needs the `covdata` tool, which a
  toolchain downloaded by `GOTOOLCHAIN` lacks ("no such tool covdata"): build it once with
  `T=$(go env GOTOOLDIR); chmod u+w "$T"; go build -o "$T/covdata" cmd/covdata`.
- The image needs Docker; without it say in the PR that the image was built only by the CI job `image`.
- Every test or lint run gets a timeout (`timeout 600 make test` or the tool's own): a hung run blocks the session.
- A local `serve` for a smoke check: `ENV=dev AUTH_MODE=insecure-dev SERVICE_NAME=assistant DB_PRIMARY_DSN=… make run`;
  `/readyz` on `HTTP_ADDR` answers when the database does and drops to 503 at SIGTERM, before `SHUTDOWN_DELAY` (FIN-289).

## Principles

Priorities when they pull apart, in this order:

1. **Test coverage** — a change without its tests (Tests below) is not done, however good the code.
2. **Clarity** — code a reader understands without the author: plain control flow, explicit names, no cleverness.
3. **Security** — the Security rules of the review are requirements, not trade-offs; this place in the order decides
   only between options that all meet them.
4. **Performance** — optimise when a measurement shows the need, not in advance; a faster but less clear version
   comes with the benchmark that justifies it.

- **No frameworks.** No web or RPC frameworks (gin, echo, fiber, go-kit), no ORM (gorm, ent), no DI containers or
  generators (fx, wire, dig), no config frameworks (viper): the standard library, grpc-go and explicit wiring in
  `internal/app`. `depguard` in `.golangci.yml` rejects the common ones.
- **Standard library first.** Use `net/http`, `log/slog`, `encoding/json`, `errors`, `testing`, `context`,
  `database/sql` interfaces and the rest of the standard library wherever they do the job; tests use `testing` (no
  testify, gomock). A third-party package only where the standard library has no equivalent — the PostgreSQL driver
  (pgx), gRPC and protobuf, protovalidate, golang-migrate, the Prometheus client, testcontainers — and a new one is
  named in the PR with why the standard library is not enough.
- **SOLID, the Go way:**
  - single responsibility — a package and a type have one reason to change: a usecase does not format messages, a
    handler does not hold business rules, a repository does not decide;
  - open/closed — new behaviour is a new type behind an existing interface (a repository, a client, a job), not a
    flag or a type switch in old code;
  - Liskov — every implementation of an interface keeps its contract, fakes in tests included: the same errors for
    the same cases, no panics where the real one returns an error;
  - interface segregation — small interfaces declared by the consumer with only the methods it calls;
  - dependency inversion — usecases depend on those interfaces, never on adapters; `wire()` passes the
    implementations in.

## Writing code

- Code, comments, identifiers, commit messages — English. `docs/*.md`, `README.md`, PR title and description —
  Russian. Comments where the logic is not obvious; a doc comment on every exported identifier.
  Exception: `COMMENT ON TABLE` and `COMMENT ON COLUMN` in migrations are Russian, one for every table and
  column (owner's request, FIN-114; `docs/migrations.md`, «Описания таблиц и столбцов»).
- Layers: `domain` ← `<feature>` ← adapters ← `cmd`. A usecase declares the interface it needs (consumer side) and
  never imports an adapter, pgx or gRPC; a handler converts messages and calls a usecase; `wire()` connects them.
- Errors to callers: only `finerr` constructors (`finerr.NotFound()`, `finerr.LimitExceeded(resetsAt)`).
  `status.Error`, `status.New`, `errdetails.ErrorInfo` are forbidden by the linter; an internal refusal is a bare
  code through `bare()` in the server package. A new client reaction needs a new code in `contracts` first. A
  handler may return a `finerr` error wrapped with `%w` or any other error: the interceptor sends the caller the
  unwrapped status or a bare `INTERNAL` (`CANCELLED` / `DEADLINE_EXCEEDED` for context errors) and logs the
  original, so no text of a wrapped error leaves the process (`docs/server.md`, step 10).
- Identity: the user of a user call is `reqctx.UserID(ctx)` (the verified `sub`), never a request field; a system
  call takes `user_id` from the request. Every new method gets its row in `AccessPolicy()`, copied from
  grpc-api.md 3.3; the server does not start without it.
- **SQL construction:** write fixed-structure queries as plain SQL with parameters (`$1`, `$2`, … for
  PostgreSQL). Use [Squirrel](https://github.com/Masterminds/squirrel) (`github.com/Masterminds/squirrel`) only
  when assembling a query dynamically, such as optional predicates or varying column lists. Changing argument
  values alone does not make a query dynamic. This limited use is an approved exception to "standard library
  first". Parameterize values; keep identifiers and structural fragments constant or from an explicit allowlist.
  Use `PlaceholderFormat(sq.Dollar)` for PostgreSQL builders. If a dynamic query cannot be expressed by the
  builder, explain the limitation beside its manual construction and preserve parameterization.
- Database: SQL with parameters only (`$1`), table and column names are constants. A usecase opens the transaction
  with `db.Run`; repositories take `db.Writer(ctx)` / `db.Reader(ctx)`; no calls to other services inside `Run`.
  `postgres.DB` delegates pool lifecycle to `go-postgres v0.3.0` and uses one transaction manager; it never adopts
  another manager's context. `RunMapped` takes an operation-specific mapper from the service adapter for expected
  FK/UNIQUE query and COMMIT errors; the library owns no domain/contract mapping. No such mapping is needed by
  current assistant operations: unknown violations stay internal. Dialogue reads use `RunWith` on primary with
  REPEATABLE READ READ ONLY, joining a same-manager outer transaction with its existing options when present.
  Migrations follow `docs/migrations.md` (two releases for an incompatible change, `CONCURRENTLY` alone in a file).
  The schema holds only primary keys, unique constraints and indexes (partial ones included), foreign keys (with
  their cascades), `NOT NULL` and `DEFAULT`: no `CHECK`, `EXCLUDE`, triggers or stored functions (the owner's
  decision of 19.10.2026); value and state rules live in the service code (`docs/schema.md`, «Инварианты в коде
  сервиса»), and `pg_notify` is called by the code in its own write transaction. A change of the schema updates
  `docs/schema.md` and the tests of its keys and queries in `migrations/schema_test.go` in the same PR.
- Clients of other services: `client.Dial` with `Retry` listing only reads and methods idempotent by construction;
  the context carries the deadline.
- Time: "now" in business logic comes from an injected clock (`func() time.Time` or an interface passed by `wire()`),
  never `time.Now()` inside a usecase or repository query: tests pin the date, and month and year boundaries must be
  testable. Calendar days and months are computed in the user's profile time zone (grpc-api.md 8), never in
  `time.Local` or the server's zone; `now()` in SQL only for technical columns (`created_at`, outbox schedule).
  Durations for metrics and timeouts may use `time.Now()` / `time.Since`.
- Money: integer minor units (`int64`, `_minor`), never floats; the sign follows the contract.
- Tolerance of what the service does not know yet: an enum value of another service it does not know (newer
  producer) follows the rule of that field in grpc-api.md 8 — hide the field or fail the call with an alert — never
  a panic or a silent default that changes meaning; an unknown field is ignored (protobuf does it). A new enum
  value or error metadata key follows the rollout order of grpc-api.md 5.1 and 8.
- Dev-only hooks — `AUTH_MODE=insecure-dev`, dev verifiers and tokens, reflection, test helpers — stay unreachable
  outside `ENV=dev`: config rejects them, and code must not add another way around it.
- Logs: `slog` `*Context` methods; no tokens, DSNs, statement contents or assistant dialogue in logs or errors.
  The debug log of a request masks `token` fields and clears every `bytes` field; other personal data in a
  request is logged at `debug` as is. Metric labels: bounded values only — never `user_id`, an operation ID or
  free text.
- Document in moderation: a doc comment on exported identifiers of packages others use, comments where the logic is
  not obvious (accounting rules, idempotency, locks, transaction boundaries), not on every function.

## Tests

Every change comes with its automated tests in the same PR — not in a follow-up. Three levels, each mandatory:

- **Unit tests — all new code.** Every new or changed function or method with logic (domain rules, usecases,
  mappers, validation, config, helpers, interceptors) has unit tests in its package: no database, no network, fakes
  of the interfaces the code declares; the main path and every branch and error the code handles.
- **Integration tests — all new functionality.** Every new or changed behaviour is tested against its real
  dependencies: repositories, migrations, transactions and locks against PostgreSQL (`testdb.DSN`), not mocks of
  pgx — the SQL itself is what is tested; clients of other services against a fake gRPC server on bufconn
  (retries, deadlines, metadata); jobs and the outbox through a real run. Every migration passes up → down → up
  (`migrations/migrations_test.go`).
- **API tests — all new functionality.** Every new or changed RPC is tested through the service's API as a caller
  sees it: `apitest.Start(t)` runs the whole assembly (`internal/app`: interceptors, access table, handlers,
  PostgreSQL with migrations) and the test calls it with the generated client as `bank-bff` (`apitest.User`) or
  another service (`apitest.Caller`). Assert the status code, `ErrorInfo.reason` (`apitest.Reason`) and metadata, and
  the response fields against the contract — not the message text. Cover the main path, every error the contract
  lists for the method, a foreign caller, another user's object and a deleted user.
- **API tests in `bank-bff` — every REST endpoint.** `bank-bff` is covered by API tests too, at its own API — the
  REST of `openapi.yaml` the mobile client calls: a real HTTP request to the `bank-bff` handler (`httptest`), the
  services behind it played by fake gRPC servers on bufconn that answer with success and with each registry error.
  Every response is validated against `openapi.yaml` (kin-openapi), and the test asserts the HTTP status,
  `error.code`, `details[]`, headers (`Retry-After`, `Idempotency-Key` handling) and SSE events — the main path, every
  error `docs/api.md` lists for the endpoint (including the translation of each `reason` by `finerr.ToREST` and an
  unknown one into 500), a missing or bad token, a missing `Idempotency-Key`, a body that fails the schema, an
  unsupported client version. Every operation of `openapi.yaml` that `bank-bff` serves has at least one API test.

Also:

- A bug fix comes with a test that fails without the fix and reproduces the reported case, at the lowest level that
  shows it, plus an API test when the caller saw the bug.
- Security-relevant behaviour (foreign caller, another user's object, deleted user, consent gates, hostile input)
  has negative tests: what must not happen is asserted — the handler did not run, no row was written, nothing leaked.
- Tests assert behaviour; a test that only executes lines or asserts on a fake it set up itself is not coverage.
  Run with `-race`; no sleeps for synchronisation — wait on a channel or a condition with a deadline.
- Each of unit, integration and API tests independently covers at least 80% of statements in the whole module
  (`make cover`). Combined coverage is supplementary and never offsets a failing suite. The pinned tool's
  rounding applies: 79.95% is accepted as 80% (FIN-238). This numerical gate does not prove scenario completeness.

## Checks

CI (table in `README.md`): `go mod tidy` clean, `go vet`, three independent `go test -race -count=1` suites with
PostgreSQL 18 and statements thresholds, Python script tests, golangci-lint, image build and migration checks.
The existing `go` job is the required aggregate: all collection and threshold steps must succeed. Run `make test`,
`make cover` and `make lint` before pushing. Tests use a process timeout; the collection commands use `-timeout=8m`.

`make test` and CI also run the Python script tests and check CHANGELOG format. Service releases follow
`docs/releasing.md`: prepare a dated version section in a reviewed PR, wait for post-merge CI, then the owner
pushes the tag. Agents never create, move or delete version tags. Release publishes GitHub metadata only;
image publication and deployment remain separate. Existing releases are preserved after validating the tag.

### Coverage reports (FIN-238)

Name every Go test `TestUnit…`, `TestIntegration…` or `TestAPI…` according to what it actually assembles.
Unit tests use isolated functions and fake declared ports, never a database/network; direct handler calls are unit,
not API. Unit tests may fake declared pgx interfaces to check Go branching and error/result translation; SQL
semantics, transaction effects and locks still require real PostgreSQL integration tests. Integration tests use
PostgreSQL, migrations, real transport clients or background processes. API tests
start the full service and call its public gRPC API with generated clients. A test helper in an API package is still
unit when it only checks metadata; a cleanup job checked through SQL without a public RPC is integration.

Name fuzz targets `FuzzUnit…`, `FuzzIntegration…` or `FuzzAPI…` (the seed corpus runs in that suite). `make test-names`
rejects unclassified Test and Fuzz names and any `Example…` before CI collection (FIN-290): the service has no public
Go API, and an example cannot carry a suite prefix. Collectors select `^(Test|Fuzz)<Suite>`; all existing tests remain selected.
`make collect-unit`, `collect-integration`, `collect-api` produce `unit.out`, `integration.out`, `api.out` and native
Go JSON event logs. Each uses `-race -count=1 -coverpkg=./...`, the same entire module, with no coverage path or
annotation exclusions. Generated production code and executable test infrastructure in `internal/app/apitest`
and `testdb` are included equally. Go excludes `_test.go`; files with no executable statements add no weight.

`make check-unit`, `check-integration`, `check-api` use `go-test-coverage/v2@v2.20.1` with `total: 80` independently.
The tool needs Go 1.27; `go run` downloads that toolchain without changing the service's Go version. Before checking,
`scripts/check-suite.sh` uses jq to require a passed Test event and reject failed/skipped tests; skipped packages
without tests are permitted. A grep guard rejects missing, empty, header-only and zero-statement profiles. With
`CI=true`, unavailable PostgreSQL fails instead of skipping. These guards validate execution, never calculate coverage.

`make cover-unit`, `cover-integration`, `cover-api` collect and check one suite; `make cover` collects all and enforces
all three thresholds. `make cover-combined` asks the tool to merge the existing profiles using its comma-separated
`--profile` option. It is informational. Reports and profiles are retained in the CI `coverage` artifact and run
summary even when a threshold fails. No token write permission or PR comment is needed, including for fork PRs.

## Workflow

- Branches: `<kind>/<slug>` — `feature/…`, `fix/…`, `tooling/…`; a PR targets `main`. Commits: imperative English
  subject (`Add …`, `Fix …`, `Address review: …`).
- Dependency updates come from Renovate (`renovate.json`; README «Библиотеки и обновления»): its `renovate/…`
  branches are the exception to the branch names and need no YouTrack issue while they change nothing but
  dependencies. FIN-211 permits merge/retry/close of allowed bot-only updates, including v0 minor/major changes,
  after required CI; deployment and rollout instructions in CHANGELOG/contracts remain the owner's responsibility.
  Human fixes make the PR manual and require a YouTrack issue. After automatic closure, create a separate fix PR
  with an issue rather than pushing to the closed Renovate branch; follow README dependency policy.
- PR title and description in Russian: what changed and why, deviations from the contracts or docs, how it was
  checked. A PR for a single issue starts its title with the key (`FIN-42 Поиск операций по сумме`); a PR for several
  issues lists all of them in the description, each linked (`https://finassist.youtrack.cloud/issue/FIN-<n>`).
- A dependency on a contract change: the contract PR is merged and `gen/go` tagged first, then the service PR bumps
  the module. Order of rollout for new enum values and error metadata — grpc-api.md 5.1 and 8.
- After the PR is open and all planned changes are pushed, request a review with one PR comment in Russian addressed
  to both reviewers; it starts with `@codex @claude`:

  ```
  @codex @claude проведите ревью этого PR по Code Review Rules в AGENTS.md, включая разделы Security и Tests.
  Замечания — на русском, inline на изменённых строках. Не придирайтесь к формулировкам документации и комментариев:
  замечание по тексту только если он искажает поведение кода. Особое внимание: <что именно и почему>.
  ```

  - `@claude` — `.github/workflows/claude.yml` (runs from the default branch), Claude Opus 5.5
    (`claude-opus-5-5`, `--effort high`; FIN-212). Needs the Claude GitHub App and the `CLAUDE_CODE_OAUTH_TOKEN` secret.
  - `@codex` — the Codex GitHub integration under the owner's ChatGPT subscription. Its model is chosen by OpenAI
    and cannot be pinned from here; a pinned model would need the OpenAI API with per-token billing, which was
    declined (FIN-103).
  - Request Claude review only once `.github/workflows/claude.yml` is on `main`: its comment-triggered workflow
    runs from the default branch. Request Codex independently when its GitHub integration is available.
  - Replies in threads and other comments never mention `@codex` or `@claude`: any mention with text starts them
    again.
- Waiting for the review: react to the PR's GitHub events when the session receives them; otherwise check 3 minutes
  after the request, then every 2 minutes. A reviewer has answered when it posted its review or summary for the
  latest commit; for Codex (`chatgpt-codex-connector[bot]`) «Didn't find any major issues» or a 👍 without a comment
  also counts. A reviewer silent for 15 minutes after the last request: say so in the PR and to the owner, stop
  waiting for it. The cycle ends when both answered the latest request and no thread is open.
- Address findings in follow-up commits on the same branch, reply in the thread and resolve it, then request a
  review again. Agents never merge a PR; the owner does.

## Code Review Rules

Read by `@claude` (through `CLAUDE.md`) and by the Codex integration, which looks for this heading. Two areas get a
dedicated pass on every PR: Security and Tests.

### General

- Write all review comments in Russian (summary and inline). Identifiers and quotes stay as they are.
- Report only verified problems with a concrete failure scenario: which caller, request or data, what goes wrong.
  Skip style nits, naming preferences and wording of comments and docs unless the text misstates the behaviour.
- Weigh findings by the priorities of «Principles»: missing tests first, then clarity, then security beyond the
  mandatory rules, then performance; a performance finding needs a measurement or an evident cost (an N+1, an
  unbounded load), not a guess.
- Post each finding as an inline comment on the changed line. The summary lists what was checked, with a
  «Безопасность» part and a «Тесты» part; «not affected» is a valid answer, with one line why.
- Check that the PR references its YouTrack issue and updates `README.md`, this file and `docs/` when it changes
  what they describe.

### Security

The service handles a user's bank operations, phone number, statement files and assistant dialogue (personal data
under 152-ФЗ). A finding names what leaks or breaks and how a client, another user or a compromised service gets to
it. Check:

- Object access: every query by an object ID is scoped by the user (`WHERE user_id = $n` with the user from
  `reqctx.UserID` or the system call's `user_id`); another user's object gives `NOT_FOUND`, not its data or a
  different error that confirms it exists (IDOR / BOLA).
- Identity: the user never comes from a request field in a user call; a new method has its row in `AccessPolicy()`
  matching grpc-api.md 3.3 (callers and model); hints `x-client-ip`, `x-client-version`, `x-read-primary` are used
  only as the server package sets them; `x-client-version` never decides access or limits.
- Errors: client-facing errors only through `finerr`; `status.message`, `error` details and logs contain no SQL,
  stack traces, tokens, DSNs or other users' data; an internal refusal is a bare code.
- Secrets: tokens and DSNs only in headers, files or `*_FILE` variables, never in logs, metric labels, URLs or error
  texts; the masking list in `docs/server.md` covers new secret metadata or fields.
- SQL: parameters only; nothing from a request selects a table, column, `ORDER BY` fragment or file path.
- Untrusted input: bounds come from protovalidate annotations in `contracts`; a handler that reads a size, count or
  page relies on them or checks itself; long-running or costly work (file parsing, LLM, calls, SMS) has a limit
  and a deadline.
- Transactions: an effect and its idempotency record or outbox row are written in one `db.Run`; no external call
  inside `Run`; a deleted user's data is not written back (tombstone).
- Clients: no user token forwarded to another service; retries only for reads and idempotent methods.
- Consent: a service that sends user data to the LLM checks the assistant consent first; an account is created only
  after the personal data consent; revoking a consent or deleting the account removes data as the contract says.
- Dev authentication and transport hooks (`insecure-dev`, dev verifiers, reflection, peer dial overrides) cannot be enabled in `stage` or `prod`. The optional stage fake model uses numeric HTTP loopback with verified service/user JWT; it does not relax identity.
- Statement files and other uploads: size, element count and nesting depth are bounded; OFX 2.x (XML) is parsed with
  DTD and external entities disabled; a hostile file ends in a handled error, not a panic, a hang or memory growth.
- Trust boundaries: limits, quotas and permissions are enforced here, never left to the client or to `bank-bff`
  alone; `x-client-version` never decides access.
- Secrets in the repository: no real keys, tokens or DSNs with passwords (compose uses throwaway local ones).
- New dependencies: flag a new library that gets network, storage or data access, so the owner decides on it.

### Tests

The criterion is not a percentage of lines but whether the use cases of the changed functionality are covered; the
80% per-suite statements target in Tests is the author's minimum, and meeting it does not settle this check. For
each behaviour the PR adds or changes:

1. List its use cases from the code, the contract and the issue: the main path; alternative paths; every error a
   caller can reach (validation, not found, another user's object, consent missing, limit exceeded, a peer
   `UNAVAILABLE`, a database error, a cancelled context or an expired deadline); boundary values (zero, negative and
   very large amounts, dates at day, month and year boundaries, time zones, empty and maximal lists, long texts);
   repeated and concurrent calls (an idempotent retry with the same key, two writers at once); a deleted user.
2. Map each case to the test that covers it and check all three levels of Tests above: new code without a unit test,
   new functionality without an integration test or without an API test is a finding.
3. Post a finding for an uncovered case only when it is reachable in the changed code and its behaviour is added or
   changed by the PR; name the concrete case and the layer that should cover it. The list in step 1 is a prompt, not
   a checklist to report item by item; a case not worth a test is fine when the PR says why.

Also check that a test would fail if its case broke, that a bug fix's test reproduces the reported case, that every
migration passes up → down → up (an irreversible step marked as `docs/migrations.md` says), and that
security-relevant behaviour has negative tests.

### Consistency

- Field meanings, error codes, limits and call models match `fin-assist/contracts`; a deviation is stated in the PR.
- Names follow the existing ones; no second way to do what a package here already does (transactions, clients,
  config, logging).
- No framework, ORM or DI container; a new third-party dependency where the standard library would do is a finding.
- SOLID: business rules outside usecases, an interface declared by its implementation instead of its consumer, a
  usecase importing an adapter, or a fake that behaves unlike the real implementation is a finding.
- Metric labels stay bounded; a new metric has a help text and is listed in `docs/server.md`.

### Checklist

- New code has unit tests; new functionality has integration tests and API tests (`apitest`), see Tests.
- In `bank-bff`, a new or changed REST endpoint has API tests over HTTP with responses validated against
  `openapi.yaml`.
- A new gRPC method has its row in `AccessPolicy()` matching grpc-api.md 3.3.
- A new client-facing error goes through `finerr`; a new reason or metadata key exists in `contracts` first.
- A new query by object ID is scoped by the user and has a test with another user's ID.
- A new effect with an idempotency record or an outbox row is written in one `db.Run`, and a retry test shows it is
  not applied twice.
- A new migration follows `docs/migrations.md` and passes squawk and up → down → up.
- A new metric has bounded labels and is listed in `docs/server.md`; a new setting is declared in `Vars` and the
  tables of `docs/config.md` are regenerated, not edited by hand.
- Each suite meets its own statements threshold; coverage reports do not replace the scenario review above.

## Renovate automation

FIN-211: dependency-only Renovate PRs may be merged/closed by the reviewed `renovate-policy.yml` controller, after exact-head required CI and bot-only commit checks. Ordinary PRs and release-to-main PRs still require the owner. Do not enable native automerge or bypass protections. Preserve base policy in `renovate-base.json` (schedules, groups, allowed versions and manual Go upgrades); generated target/version approvals live on the separate `renovate-policy` branch. Failed exact values are isolated for explicit Dashboard approval; new values keep their original groups. AndroidX/Compose and mixed groups never auto-merge. See `docs/renovate-policy.md` for credentials, safeguards, bootstrap and verification.

## Stage artifacts (FIN-148)

`scripts/stage_artifact.py` extracts the binary from the smoke-tested image; `docs/stage-artifacts.md` describes provenance, checks and retention. Keep its Python tests and CI image job in sync. Artifacts do not authorize deployment; only a successful main push CI is eligible for stage. GHCR/production delivery is still pending.
