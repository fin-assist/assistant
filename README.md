# assistant

Сервис LLM-помощника «Понятные финансы»: вопросы и ответы Alice AI с вызовом функций, поток ответа, история диалога, суточный лимит вопросов, пояс пользователя для моментов в ответах. Контракт — `finassist.assistant.v1.AssistantService` (REST 7.x), `usersettings.v1.UserSettingsSyncService/SetUserTimezone`, `userdeletion.v1.UserDeletionService/PurgeUserData` в [`fin-assist/contracts`](https://github.com/fin-assist/contracts) (`docs/api.md` раздел 7, `docs/grpc-api.md` 7.4), модуль `github.com/fin-assist/contracts/gen/go`.

Создан из шаблона [`fin-assist/go-service-template`](https://github.com/fin-assist/go-service-template) ([FIN-117](https://finassist.youtrack.cloud/issue/FIN-117)); правила шаблона действуют здесь без изменений.

- [docs/schema.md](docs/schema.md) — схема БД `assistant`: сущности, ключи, правила в коде сервиса, индексы под запросы ([FIN-114](https://finassist.youtrack.cloud/issue/FIN-114)).
- [docs/server.md](docs/server.md) — gRPC-сервер и интерцепторы, идентичность, таблица доступа, клиенты, транзакции, логи и метрики.
- [docs/config.md](docs/config.md) — переменные окружения.
- [docs/tools.md](docs/tools.md) — подключённые клиенты данных и согласия.
- [docs/aistudio.md](docs/aistudio.md) — HTTP-адаптер Responses API и его ограничения.
- [docs/generation.md](docs/generation.md) — принятие/повтор, рабочий цикл, бюджеты и оставшиеся этапы.
- [docs/migrations.md](docs/migrations.md) — миграции и запросы на pgx ([FIN-115](https://finassist.youtrack.cloud/issue/FIN-115)).
- [AGENTS.md](AGENTS.md) — карта и правила для агентов, Code Review Rules.

## Автоматизация обновлений зависимостей

[Политика Renovate](docs/renovate-policy.md) (FIN-211): одна актуальная release/main, автомерж полностью ботских PR после обязательного CI, один повтор красного CI и закрытие с явным повтором через Dependency Dashboard. AndroidX/Compose остаются на ручной проверке. Исходные группировки, ограничения версий и расписания — `renovate-base.json`; generated target и quarantine — ветка `renovate-policy`. Активация требует отдельного токена и строгой защиты веток; по умолчанию выключена.

## Реализовано (FIN-220)

- `AssistantService.GetLimit`: чтение primary, `ASSISTANT_DAILY_MAX` (по умолчанию 5), сброс в полночь по Москве; профильный пояс лимит не меняет. Чтение не создаёт строку пользователя.
- `AssistantService.AskQuestion` и `RetryAnswer`: go-idempotency v0.1.0, primary-согласие user, атомарное списание/сохранение и запуск после commit. Replay не создаёт генерацию и не проверяет лимит/согласие повторно.
- Цикл Responses с пятью финансовыми функциями, проверкой всего батча, ограниченным параллелизмом и сохранением фрагментов. `present_result` строит chart/rows и чипы из сохранённого результата функции; числа и фильтры модель не задаёт. Ошибка очищает частичный ответ и возвращает попытку; NO_DATA необходимых функций или выбранный LOCKED делает complete бесплатным. Зависшие ответы завершаются фоновым заданием, удаление отменяет генерацию на любом экземпляре.
- `UserSettingsSyncService.SetUserTimezone`: только вызывающий `user`, применяется большая `settings_version`; повтор/старая версия — OK без изменения. IANA-база встроена в бинарь.
- `UserDeletionService.PurgeUserData`: только `user`, удаление корня `user_state` каскадирует к вопросам, ответам и блокам; tombstone фиксируется в той же транзакции. Повтор — OK; поздняя доставка пояса ничего не воскрешает.
- `go-identity v0.1.0`: JWT пользователя через `GetSigningKeys` сервиса user, ServiceAccount через HTTPS JWKS Kubernetes. `go-tombstone v0.1.0`: primary admission, Guard всех записей и миграции с отдельной таблицей версий.

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
internal/adapter/grpc/server/      gRPC server, interceptors, access table types, identity interfaces
internal/adapter/grpc/<feature>/   gRPC handlers: messages <-> usecases
internal/adapter/postgres/         pools, transaction manager, repositories on pgx
internal/adapter/postgres/migrate/ migration runner (migratetest: PostgreSQL for tests)
internal/adapter/client/           clients of other services, service token source
internal/worker/                   background jobs: outbox dispatcher, scheduled jobs
internal/config/                   configuration from the environment
internal/reqctx/                   request-scoped values: request ID, caller, user, hints
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

- **Подключение** — по разделу «Внедрение» README библиотеки: `go get github.com/fin-assist/go-<имя>@vX.Y.Z`, источник миграций в `MigrationSources()` до источника сервиса, сборка в `wire()` — библиотека получает от сервиса транзакцию (`db.Tx`), логгер, реестр метрик, часы и настройки.
- **Версии** — semver-теги библиотеки, `go.mod` сервиса закрепляет версию; до первого прода — `v0`, несовместимое изменение — новый MINOR с записью «Несовместимо» в CHANGELOG библиотеки. Правила — `docs/releasing.md` в `fin-assist/go-library-template`.
- **Обновления приносит Renovate** (`renovate.json`): модули `github.com/fin-assist/*` (библиотеки и `contracts/gen/go`) — отдельный PR сразу после тега, остальные зависимости — один PR в неделю, версия Go — вручную. PR Renovate проходит CI сервиса и обрабатывается по [политике FIN-211](docs/renovate-policy.md). При несовместимом обновлении человеческий коммит в открытом PR переводит его на ручное сопровождение; если ботский PR уже закрыт после повторного падения, исправления сервиса идут отдельным PR с задачей YouTrack. Порядок миграции — по CHANGELOG библиотеки.
- **Проверить библиотеку до выпуска** — локально `go mod edit -replace github.com/fin-assist/go-<имя>=../go-<имя>`; `replace` в PR сервиса не попадает.

## Проверки

CI (`.github/workflows/ci.yml`):

| Job | Что |
|---|---|
| `go` | `go mod tidy` без изменений, `go vet`, `go test -race` с PostgreSQL 18 в Docker (testcontainers), включая «вверх → вниз → вверх» для миграций и ключи и запросы схемы (`migrations/schema_test.go`); тесты скриптов; общее покрытие и покрытие изменённых строк — в сводке прогона и комментарием в PR |
| `lint` | golangci-lint (`.golangci.yml`), в том числе `forbidigo` против обхода `finerr` и `depguard` против фреймворков, ORM и DI-контейнеров |
| `image` | сборка образа и запуск бинаря в нём |
| `migrations` (PR) | существующие миграции не изменены, squawk по новым |

Локально:

```bash
make test   # Docker for testcontainers, or TEST_DATABASE_URL=postgres://... to use your PostgreSQL
make lint
```

### Покрытие изменённых строк

Цель — не ниже **80 %** изменённых исполняемых строк production-кода (без `_test.go` и `testdata/`). CI считает его скриптом `scripts/coverage-diff.py` по профилю `go test -coverpkg=./... -coverprofile=cover.out` и диффу PR против базы (`base...HEAD`) и публикует таблицу в сводке прогона и одним комментарием в PR, который обновляется на каждом пуше. Ничего не падает: порог проверяется на ревью. Непокрытые строки перечисляются в описании PR («Как проверено») с причиной — например, ветка ошибки, которую не вызвать без отказа ОС.

Строка учитывается, если её содержит блок покрытия Go с ненулевым числом statements. Это метрика диапазонов строк: комментарии и пустые строки внутри блока тоже учитываются. С `-coverpkg=./...` строка покрыта, тесты какого бы пакета её ни выполнили.

```bash
make cover   # cover.out and the table against origin/main
python3 scripts/coverage-diff.py                 # project totals
python3 -m unittest discover -s scripts -p 'test_*.py'
```

Модули `github.com/fin-assist/*` берутся из git, минуя прокси (`GOPRIVATE`, задан в `Makefile` и CI). Для приватного — токен с чтением репозиториев fin-assist: локально `git config --global url."https://x-access-token:<token>@github.com/fin-assist/".insteadOf "https://github.com/fin-assist/"`, в CI — секрет `GO_MODULES_TOKEN`, в `docker build` — секрет `netrc`.

### Общее покрытие и изменённые строки

CI публикует разделы «Покрытие проекта» и «Покрытие изменённых строк» в сводке запуска и одном обновляемом комментарии PR. Оба раздела используют один профиль корневого Go-модуля, без повторного запуска тестов; на push сохраняется общее покрытие. Комментарий для PR из fork не записывается. При ошибке построения или пустом файле отчёта последний корректный комментарий сохраняется.

Метрика — уникальные строки внутри блоков Go-профиля с ненулевым числом statements, а не процент statements из `go tool cover`. `_test.go` и `testdata/` исключены; сгенерированный production-код включён на тех же условиях. Повторные блоки разных тестовых бинарей объединяются: достаточно одного выполнения. Без исполняемых строк отчёт сообщает «Нет данных», а не 100 %. Цель **80 %** относится только к изменённым строкам и проверяется на ревью; общий процент её не заменяет.
