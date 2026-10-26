# assistant

Выпуск версии сервиса и повторная публикация GitHub Release описаны в [docs/releasing.md](docs/releasing.md). История изменений — [CHANGELOG.md](CHANGELOG.md). История начинается с пустого `Unreleased`; версии и теги создаёт владелец после PR и post-merge CI. Публикация образа и deployment — отдельные этапы.

Сервис LLM-помощника «Понятные финансы»: вопросы и ответы Alice AI с вызовом функций, поток ответа, история диалога, суточный лимит вопросов, пояс пользователя для моментов в ответах. Контракт — `finassist.assistant.v1.AssistantService` (REST 7.x), `usersettings.v1.UserSettingsSyncService/SetUserTimezone`, `userdeletion.v1.UserDeletionService/PurgeUserData` в [`fin-assist/contracts`](https://github.com/fin-assist/contracts) (`docs/api.md` раздел 7, `docs/grpc-api.md` 7.4), модуль `github.com/fin-assist/contracts/gen/go`.

Создан из шаблона [`fin-assist/go-service-template`](https://github.com/fin-assist/go-service-template) ([FIN-117](https://finassist.youtrack.cloud/issue/FIN-117)); правила шаблона действуют здесь без изменений.

- [docs/schema.md](docs/schema.md) — схема БД `assistant`: сущности, ключи, правила в коде сервиса, индексы под запросы ([FIN-114](https://finassist.youtrack.cloud/issue/FIN-114)).
- [docs/server.md](docs/server.md) — gRPC-сервер и интерцепторы, идентичность, таблица доступа, клиенты, транзакции, логи и метрики.
- [docs/config.md](docs/config.md) — переменные окружения.
- [docs/tools.md](docs/tools.md) — подключённые клиенты данных и согласия.
- [docs/aistudio.md](docs/aistudio.md) — HTTP-адаптер Responses API и его ограничения.
- [docs/generation.md](docs/generation.md) — принятие/повтор, рабочий цикл, бюджеты и оставшиеся этапы.
- [docs/server.md#ошибки-и-совместимость-fin-225](docs/server.md#ошибки-и-совместимость-fin-225) — canonical Status, независимые разрешения original RPC / новой генерации и порядок обновления потребителей (`contracts/gen/go v0.5.0`).
- [docs/migrations.md](docs/migrations.md) — миграции и запросы на pgx ([FIN-115](https://finassist.youtrack.cloud/issue/FIN-115)).
- [AGENTS.md](AGENTS.md) — карта и правила для агентов, Code Review Rules.

## Автоматизация обновлений зависимостей

[Политика Renovate](docs/renovate-policy.md) (FIN-211): одна актуальная release/main, автомерж полностью ботских PR после обязательного CI, один повтор красного CI и закрытие с явным повтором через Dependency Dashboard. AndroidX/Compose остаются на ручной проверке. Исходные группировки, ограничения версий и расписания — `renovate-base.json`; generated target и quarantine — ветка `renovate-policy`. Активация требует отдельного токена и строгой защиты веток; по умолчанию выключена.

## Реализовано (FIN-220)

- `AssistantService.GetLimit`: чтение primary, `ASSISTANT_DAILY_MAX` (по умолчанию 5), сброс в полночь по Москве; профильный пояс лимит не меняет. Чтение не создаёт строку пользователя.
- `AssistantService.AskQuestion` и `RetryAnswer`: go-idempotency v0.6.0, primary-согласие user, атомарное списание/сохранение и запуск после commit. Replay не создаёт генерацию и не проверяет лимит/согласие повторно.
- Цикл Responses с пятью финансовыми функциями, проверкой всего батча, ограниченным параллелизмом и сохранением фрагментов. `present_result` строит chart/rows и чипы из сохранённого результата функции; числа и фильтры модель не задаёт. Ошибка очищает частичный ответ и возвращает попытку; NO_DATA необходимых функций или выбранный LOCKED делает complete бесплатным. Зависшие ответы завершаются фоновым заданием, удаление отменяет генерацию на любом экземпляре.
- `UserSettingsSyncService.SetUserTimezone`: только вызывающий `user`, применяется большая `settings_version`; повтор/старая версия — OK без изменения. IANA-база встроена в бинарь.
- `UserDeletionService.PurgeUserData`: только `user`, удаление корня `user_state` каскадирует к вопросам, ответам и блокам; tombstone фиксируется в той же транзакции. Повтор — OK; поздняя доставка пояса ничего не воскрешает.
- `go-identity v0.2.0` (`NewAssembly`, FIN-324): JWT пользователя через `GetSigningKeys` сервиса user; сервисные токены — ServiceAccount через HTTPS JWKS Kubernetes при `ENV=prod` или короткие JWT стенда по файлам ключей `AUTH_STAGE_*` при `ENV=stage`. `go-tombstone v0.1.0`: primary admission, Guard всех записей и миграции с отдельной таблицей версий.

- `AssistantService.ListMessages`: согласованный снимок primary, страницы до 50 сообщений (по умолчанию 20), старые сообщения первыми; заменённые ответы скрыты. Актуальный ответ остаётся рядом со своим вопросом. `changed_since` вычисляется одним `GetDataVersion` на страницу с источниками.
- `AssistantService.WatchAnswer`: сначала сохранённый снимок, затем новые фрагменты, блоки, источник и чипы; завершение возвращает актуальный остаток лимита. Переподключение начинает с нового снимка. Один слушатель PostgreSQL на экземпляр приложения, включая уведомление об удалении пользователя.

История с источниками требует `PEER_BANK_STATEMENT_DATA_ADDR=<адрес>`: без доступного сервиса версий такой запрос завершается ошибкой, вместо недостоверного пустого `changed_since`. Пустая история и ответы без источников не требуют соседа. Неизвестные тип/версия сохранённого блока передаются как текст `alt_text`, сохраняя индекс; отсутствие пригодного текста — ошибка данных. Текущий gRPC-контракт ограничивает набор структурированных блоков.

Цикл использует ограниченный HTTP/SSE-транспорт: stateless-продолжение, store=false и отключение логирования на каждом запросе. Бюджет — расчётные 24 000 токенов на шаг / 48 000 на вопрос и строгие 64/128 КиБ полного JSON, включая повторный контекст. Выход — 4096 токенов, 8 шагов, 16 функций, до 4 одновременно, 3 поиска, общий срок 3 минуты. Пустой ASSISTANT_MODEL_URI оставляет чтение доступным, а новые вопросы/повторы завершаются UNAVAILABLE без списания. Подробности — [docs/generation.md](docs/generation.md).

Цитаты веб-поиска выводятся отдельным текстовым списком из provider annotations. Готовые блоки сохраняются до выдачи и сохраняют порядок с последующим текстом; завершение не меняет уже показанный график. OFF_TOPIC завершает цикл без попытки и без повтора. После 5 исходов за скользящие 168 часов новые Ask/Retry получают RATE_LIMITED; успешный complete charged=true сбрасывает этот отдельный счётчик. **Оставшиеся этапы:** сценарный фейк и сверка с провайдером — FIN-142. Общий стенд с реальными соседями пока не проверен. Полная готовность помощника ещё не заявляется. История — последние полные завершённые пары до 16 000 расчётных токенов, без суммаризации.

`AUTH_MODE=verify` требует доступных ключей user, issuer/audience, Kubernetes CA/JWKS и двух отдельных файлов токенов: для Kubernetes API и `finassist-internal`; настройки — [docs/config.md](docs/config.md). Верификация не делает fallback на dev при ошибке. Обновление ключей и исходящий токен учитывают ротацию; ресурсы закрываются с приложением. Режим подписанных файловых сервисных токенов отдельного systemd-стенда пока не подключён: пример unit не означает готовность stage. `insecure-dev` допускается только при `ENV=dev`, в том числе при прямом вызове сборки.

## Раскладка

`internal/asking` принимает вопросы и повторы, `internal/generation` выполняет цикл модели и инструментов. `AssistantConsent` и `FinancialData` подключены в wire(); клиенты и вся сборка проверяются через gRPC на bufconn и локальный HTTP Responses.

Внутренние границы ([FIN-232](https://finassist.youtrack.cloud/issue/FIN-232)): адаптеры нормализуют типизированные финансовые факты отдельно от JSON модели; представление использует эти факты. Политика OFF_TOPIC выбирает допуск и терминальные эффекты в сценариях сервиса, PostgreSQL атомарно сохраняет результат. Реестр инструментов объединяет схему, проверку, выполнение и допустимые имена метрик. Восстановление зависших ответов зависит только от своего узкого интерфейса хранилища.

```
cmd/assistant/                     main: serve and migrate commands, listeners, shutdown
internal/app/                      assembly: identity, pools, server, wire() (handlers, clients, jobs), access table, migration sources
internal/app/apitest/              the whole service in-process for API tests
internal/domain/                   entities and invariants, no infrastructure imports
internal/<feature>/                usecases; interfaces of repositories and clients they need
go-grpc/server                    shared gRPC server, interceptors and identity interfaces
internal/adapter/grpc/<feature>/   gRPC handlers: messages <-> usecases
internal/adapter/postgres/         pools, transaction manager, repositories on pgx
go-migrate                       shared migration runner and public migratetest harness
internal/testdb/                  test-owned PostgreSQL server selection
internal/adapter/client/           financial-data adapters over go-grpc/client
go-worker                        shared background job lifecycle
internal/config/                   configuration from the environment
go-grpc/reqctx                    shared request ID, caller, user and primary-read hints
internal/logx/                     slog with values from the context
internal/ops/                      /metrics, /healthz, /readyz, readiness
migrations/                        SQL migrations, embedded into the binary
deploy/systemd/                    example unit for the stand
```

Раскладка — по руководству go.dev «Organizing a Go module» (серверный проект), слои и интерфейсы на стороне потребителя — по go-clean-arch v4: сценарий в `internal/<feature>` объявляет интерфейсы нужных ему репозиториев и клиентов, адаптеры их реализуют, связывает всё `wire()`.

## Запуск

```bash
# PostgreSQL 18, migrations and the service in Docker (dev mode, docs/config.md)
make up
# the same service on the host against the compose PostgreSQL
make run
```

- **Образ** — `Dockerfile`: статический бинарь на distroless, пользователь `nonroot`. Один бинарь: `serve` в Deployment, `migrate up` в Job. Публикация в GHCR — FIN-148.
- **Стенд** (FIN-149) — тот же бинарь под systemd: пример unit-файла с секретами через `LoadCredential` — [deploy/systemd/assistant.service](deploy/systemd/assistant.service). Скрипт деплоя запускает `migrate up` перед перезапуском unit.
- **k3s** — манифесты в `fin-assist/infra` (FIN-147): ServiceAccount, projected token, headless Service, пробы `grpc`.

## Библиотеки и обновления

Общий код сервисов — отдельные Go-библиотеки из шаблона [`fin-assist/go-library-template`](https://github.com/fin-assist/go-library-template) ([FIN-118](https://finassist.youtrack.cloud/issue/FIN-118)): [`go-identity`](https://github.com/fin-assist/go-identity) (FIN-119), [`go-idempotency`](https://github.com/fin-assist/go-idempotency) (FIN-78), [`go-outbox`](https://github.com/fin-assist/go-outbox) (FIN-109), [`go-tombstone`](https://github.com/fin-assist/go-tombstone) (FIN-110).

`go-grpc v0.2.0` предоставляет server/client/reqctx, `go-migrate v0.2.0` — миграции и публичный тестовый harness, `go-worker v0.1.1` — запуск фоновых заданий ([FIN-256](https://finassist.youtrack.cloud/issue/FIN-256)). Общая инфраструктура подключена по выпущенным тегам; финансовые клиенты, Alice AI adapter, лимиты, OFF_TOPIC и диалог остаются в сервисе.

`go-postgres v0.3.0` управляет пулами и транзакциями через тонкий `postgres.DB` ([FIN-240](https://finassist.youtrack.cloud/issue/FIN-240)): lifecycle пулов общий, read routing остаётся у сервиса, idempotency/tombstone используют тот же `Tx(ctx)`. `RunMapped` принимает обработчик ожидаемых ограничений из адаптера операции; текущим операциям assistant новые соответствия доменным ошибкам не нужны. Чужой транзакционный контекст отклоняется, а чтение диалога использует общий `RunWith` с сохранением primary и snapshot-опций; подробности — [docs/server.md](docs/server.md#транзакции-и-чтение).

- **Подключение** — по разделу «Внедрение» README библиотеки: `go get github.com/fin-assist/go-<имя>@vX.Y.Z`, источник миграций в `MigrationSources()` до источника сервиса, сборка в `wire()` — библиотека получает от сервиса транзакцию (`db.Tx`), логгер, реестр метрик, часы и настройки.
- **Версии** — semver-теги библиотеки, `go.mod` сервиса закрепляет версию; до первого прода — `v0`, несовместимое изменение — новый MINOR с записью «Несовместимо» в CHANGELOG библиотеки. Правила — `docs/releasing.md` в `fin-assist/go-library-template`.
- **Обновления приносит Renovate** (`renovate.json`): модули `github.com/fin-assist/*` (библиотеки и `contracts/gen/go`) — отдельный PR сразу после тега, остальные зависимости — один PR в неделю, версия Go — вручную. PR Renovate проходит CI сервиса и обрабатывается по [политике FIN-211](docs/renovate-policy.md). При несовместимом обновлении человеческий коммит в открытом PR переводит его на ручное сопровождение; если ботский PR уже закрыт после повторного падения, исправления сервиса идут отдельным PR с задачей YouTrack. Порядок миграции — по CHANGELOG библиотеки.
- **Проверить библиотеку до выпуска** — локально `go mod edit -replace github.com/fin-assist/go-<имя>=../go-<имя>`; `replace` в PR сервиса не попадает.

## Проверки

CI (`.github/workflows/ci.yml`):

| Job | Что |
|---|---|
| `go` | `go mod tidy` без изменений, `go vet`, три независимых набора `go test -race -count=1` с PostgreSQL 18 (testcontainers), включая миграции и схему; для каждого не ниже 80% statements; тесты скриптов; отчёты и профили в artifact `coverage` и сводке прогона |
| `lint` | golangci-lint (`.golangci.yml`), в том числе `forbidigo` против обхода `finerr` и `depguard` против фреймворков, ORM и DI-контейнеров |
| `image` | сборка образа и запуск бинаря в нём |
| `migrations` (PR) | существующие миграции не изменены, squawk по новым |

Локально:

```bash
make test   # Docker for testcontainers, or TEST_DATABASE_URL=postgres://... to use your PostgreSQL
make lint
```

### Три независимых уровня покрытия (FIN-238)

Каждый набор обязан независимо покрывать **не менее 80% statements всего production-кода сервиса**.
Суммарный результат не компенсирует недостаток unit, integration или API. Метрика Go statements заменяет прежний
самописный подсчёт изменённых строк; с ним проценты напрямую не сравниваются. Версия `go-test-coverage/v2` закреплена
на `v2.20.1`, применяется её округление: **79,95% принимается как 80%** по решению владельца.

- `TestUnit…` — изолированные функции/сценарии с фейками портов, без БД и сети; прямой вызов handler относится сюда.
- `TestIntegration…` — настоящие PostgreSQL 18, миграции, транзакции/блокировки, transport clients и фоновые процессы.
  Unit может использовать fake объявленного `pgx.Tx` для Go-веток и преобразования ошибок/результатов; семантика SQL
  и эффекты транзакций этим не доказываются и остаются в настоящих PostgreSQL integration-тестах.
- `TestAPI…` — полная сборка сервиса и публичный gRPC-вызов сгенерированным клиентом, включая реальные зависимости.

Все три набора используют `-race -count=1 -timeout=8m -coverpkg=./...` и одинаковый знаменатель всего модуля.
Исключений путей/аннотаций нет: generated production-код и исполняемая тестовая инфраструктура `apitest`/`testdb`
входят во все профили на одинаковых условиях. `_test.go` исключает сам Go; неисполняемые файлы не добавляют statements.

```bash
make collect-unit          # unit.out + unit.jsonl, without the numerical gate
make check-unit            # native statements report + total >=80%
make cover-unit            # collect and check; also cover-integration / cover-api
make cover                 # all three suites and gates
make cover-combined        # informational union of existing profiles
```

`make test-names` и CI отклоняют Test- и Fuzz-функции без префикса уровня (`FuzzUnit…`/`FuzzIntegration…`/`FuzzAPI…`, seed-корпус выполняется в своём наборе) и любые `Example…`, чтобы новые тесты не выпадали из выборок (FIN-290). Сборщики выбирают `^(Test|Fuzz)<Уровень>`.

Нужны jq, Go с автоматической загрузкой toolchain и PostgreSQL/Docker (`TEST_DATABASE_URL` для своего сервера).
Сам инструмент покрытия требует Go 1.27; `go run` загружает его отдельно от версии Go сервиса. Отсутствие PostgreSQL
при `CI=true` — ошибка. Проверка `scripts/check-suite.sh` требует хотя бы один успешно выполненный тест, не допускает
пропущенных/упавших Test-событий и профиля без исполняемых statements; пакет без тестов не считается пропуском теста.
Пустой suite, пустой/отсутствующий профиль и один заголовок `mode:` не подтверждают покрытие.

В CI job `go` собирает и проверяет каждый уровень отдельно, сохраняет `*.out`, нативные JSON-логи, breakdown и текстовые
отчёты в artifact `coverage` даже при красном пороге. Сводка различает unit/integration/api/combined.
Суммарный результат объединяет профили штатным comma-separated `--profile` инструмента, без сложения процентов.
Итоговый обязательный `go` падает при ошибке любого сбора или порога. PR-комментарии не записываются; токен только для
чтения, ограничения fork PR сохраняются.

Покрытие statements не доказывает полноту сценариев. Общий стенд с развёрнутыми соседями и реальный AI Studio не
проверены; сценарный сервер и сверка провайдера остаются в FIN-142 ([границы проверок](docs/aistudio.md)). Проверить
связь сценариев с Qase в этой сессии не удалось: `qql_search` возвращает `Business plan required`; это ограничение
чтения, не доказательство отсутствия кейсов.

Модули `github.com/fin-assist/*` берутся из git, минуя прокси (`GOPRIVATE`, задан в `Makefile` и CI). Для приватного — токен с чтением репозиториев fin-assist: локально `git config --global url."https://x-access-token:<token>@github.com/fin-assist/".insteadOf "https://github.com/fin-assist/"`, в CI — секрет `GO_MODULES_TOKEN`, в `docker build` — секрет `netrc`.

В FIN-256 дополнен настоящий HTTP/SSE integration-прогон через `GenerationModel`: повреждённый lifecycle, чужие идентификаторы, несогласованные function arguments/final JSON, usage и refusal. Ошибка не возвращает terminal output/usage/tool calls, частичный текст остаётся предварительным; выполняется один POST и одно резервирование без платного повтора. FIN-262 исправляет отдельное падение library up/down/up; само исправление миграционного harness не подтверждает порог integration. Подключён опубликованный `go-migrate v0.2.0` с исправлением FIN-262; проверки работают на основном модуле без временных `-modfile`/`replace`.
