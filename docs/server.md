# gRPC-сервер, клиенты, наблюдаемость

Правила межсервисного взаимодействия — в `fin-assist/contracts`, [`docs/grpc-api.md`](https://github.com/fin-assist/contracts/blob/main/docs/grpc-api.md) (разделы 3, 4, 5, 9). Общая реализация подключена из `go-grpc v0.2.0` (FIN-256); ниже — её подключение и правила сервиса.

## Порядок интерцепторов

Каждый вызов (unary и поток) проходит по порядку:

1. **`x-request-id`** — из metadata или новый; значение неожиданной формы (не `[A-Za-z0-9._-]{1,128}`) заменяется; в debug-журнал вызова такое значение попадает как `(rejected)`, а не как есть (go-grpc v0.2.0, FIN-300).
2. **Логи и метрики** — после обработчика (см. «Наблюдаемость»).
3. **Перехват паники** — голый `INTERNAL`, в лог только метод и факт паники без её payload и стека.
4. **Сервисный токен** `x-service-authorization` → имя вызывающего. Нет или неверный — голый `UNAUTHENTICATED`.
5. **Таблица доступа** (`AccessPolicy()` в `internal/app/wire.go`, копия таблицы grpc-api.md 3.3). Метода нет в таблице или вызывающего нет в правиле — голый `PERMISSION_DENIED`, строка в лог уровня `ERROR` и метрика `finassist_grpc_denied_total{cause="caller_not_allowed"}` (на неё — алерт). Сервер **не стартует**, если у зарегистрированного метода нет правила.
6. **Подсказки `bank-bff`** — `x-client-ip`, `x-client-version`, `x-read-primary` берутся только от `bank-bff`; от других вызывающих игнорируются.
7. **Токен пользователя** `authorization` — по модели метода:

   | Модель | `authorization` | Пользователь в обработчике |
   |---|---|---|
   | `ModelUser` | обязателен; нет — `INVALID_TOKEN` | `reqctx.UserID(ctx)` = `sub` |
   | `ModelMixed` | есть — проверяется; у `bank-bff` без проверенного токена — `INVALID_TOKEN` (go-grpc v0.2.0, FIN-299), у остальных без `authorization` — системный вызов | `UserID` или поле `user_id` запроса; в `AccessPolicy()` assistant смешанных методов нет |
   | `ModelSystem` | игнорируется | поле `user_id` запроса |
   | `ModelNoUser` | игнорируется | нет (вход: `AuthService`) |

   Ошибки проверки — из реестра (`INVALID_TOKEN`, `TOKEN_EXPIRED`, `TOKEN_REVOKED`), они доходят до клиента. Токен, принятый верификатором без `sub`, — тоже `INVALID_TOKEN`: пользовательский вызов не превращается в системный.
8. **Tombstone** — удалённый пользователь в вызове от его имени получает `TOKEN_REVOKED`. Сбой проверки — голый `UNAVAILABLE` (обработчик ещё не выполнялся, повтор безопасен). Системные вызовы для удалённого пользователя обрабатывает сам обработчик: OK без действий (grpc-api.md 6.1).
9. **protovalidate** — нарушения аннотаций `.proto` → `VALIDATION_ERROR` с `BadRequest` по полям; `reason` нарушения: `required` → `REQUIRED`, формат (`pattern`, `email`, `uuid`, `prefix`, `defined_only`, …) → `INVALID_FORMAT`, остальное (длины, диапазоны, размеры, CEL) → `OUT_OF_RANGE`. В потоках проверяется каждое входящее сообщение.

10. **Нормализация ошибки обработчика** — вызывающему уходит только статус: обёрнутый `fmt.Errorf("…: %w", finerr.NotFound())` разворачивается до самого статуса (текст обёртки не уходит), ошибка без статуса (`pgx`, `io`, своя) становится голым `INTERNAL`, `context.Canceled` / `DeadlineExceeded` — голыми `CANCELLED` / `DEADLINE_EXCEEDED`. Без этого grpc-go отправил бы `UNKNOWN` с `err.Error()` в `status.message`. Лог содержит безопасные атрибуты статуса; исходный текст ошибки и payload паники общая библиотека не записывает.

Голые коды без `ErrorInfo` (шаги 3–5, 8, 10) — внутренние отказы, `bank-bff` отдаёт по ним 500 (grpc-api.md 5.1, правило 3). Строит их `bare()` в общей библиотеке `go-grpc/server`; узкий `account/unavailable.go` сохраняет прежний bare UNAVAILABLE до admission при выключенной модели; остальные ошибки — конструкторы `finerr`, обход запрещён линтером (`forbidigo`, `.golangci.yml`).

Health (`grpc.health.v1`) и reflection (только `ENV=dev`) открыты без токенов.

## Идентичность и удаление

`AUTH_MODE=verify` собирается одним вызовом `identity.NewAssembly` из `go-identity v0.2.0` (FIN-324; `internal/app/identity.go` только переносит переменные в опции). Пользовательский access JWT проверяется по Ed25519-ключам из authenticated `user.UserInternalService/GetSigningKeys` (соединение — `client.Dial` к `PEER_USER_ADDR`, повтор только `GetSigningKeys`, исходящий токен сборки); issuer/audience задаются явно. Сервисные токены:

- `ENV=prod` — ServiceAccount Kubernetes: подпись, issuer, audience `finassist-internal`, namespace и согласованность claims по JWKS API-сервера через HTTPS с доверием только к cluster CA; API-токен чтения JWKS и исходящий internal-токен (`SERVICE_TOKEN_FILE`) — разные файлы. Файлы стенда в prod — ошибка старта.
- `ENV=stage` — короткие JWT стенда: входящие проверяются по `AUTH_STAGE_PUBLIC_KEYS_FILE` (`kid` = имя сервиса, ключ привязан к одному сервису) и `AUTH_STAGE_ISSUER`, исходящие подписываются `AUTH_STAGE_SIGNING_KEY_FILE` (`kid` = `sub` = `SERVICE_NAME`). Файлы читаются при старте; ротация ключа — перезапуском.
- `ENV=dev` — `verify` не допускается (конфигурация и сборка отказывают), только `insecure-dev`.

`PEER_USER_ADDR` обязателен. Ключи обновляет фоновое задание `identity-keys` (user — 10 мин, cluster — 1 час); до его первого цикла ключи загружаются по первому запросу. При остановке задание завершается вместе с остальными, затем `App.Close` закрывает соединение с user. По сравнению с прежней собственной сборкой (v0.1.0) проверка токенов та же; добавлены stage-режим, метрика `finassist_identity_jwks_skipped_keys{source}` и таймауты TLS/заголовков JWKS; пустые `AUTH_KUBERNETES_*` при вызове сборки без `config.Load` получают значения по умолчанию библиотеки (через `config.Load` они всегда заданы).

`AUTH_MODE=insecure-dev` разрешён только при `ENV=dev`: `Bearer dev:bank-bff` в `x-service-authorization`, `Bearer dev:<user_id>` в `authorization`. Пользовательские ID работающих методов — UUID. Проверка dev-ограничения есть и в конфигурации, и в сборке. Stage без файлов ключей или без `PEER_USER_ADDR` не стартует.

`go-tombstone v0.1.0` читает primary без кеша при admission. Удалённый пользователь получает TOKEN_REVOKED; сбой чтения — UNAVAILABLE до обработчика. Каждый путь записи берёт `Guard` в сервисной READ COMMITTED транзакции до блокировок строк. Системный получатель после удаления отвечает OK без эффекта. `PurgeUserData` удаляет `user_state` и каскадные записи и фиксирует tombstone одной транзакцией, повтор не меняет результат. Tombstone бессрочный, миграции библиотеки идут перед сервисом в отдельной таблице версий.

FIN-220 реализует все семь RPC, включая AskQuestion/RetryAnswer с циклом модели и серверным построением блоков. При ненастроенной модели новые вопросы/повторы завершаются UNAVAILABLE без списания. История и снимки потока читаются с primary в REPEATABLE READ, без внешних вызовов внутри транзакции. Версии для `changed_since` запрашиваются после чтения: один GetDataVersion на страницу с источниками, в потоке — для нового источника. Сбой этого чтения завершает RPC, не создавая ошибку модели. Принятие, возвраты, отмена, бюджеты и оставшиеся этапы — в [generation.md](generation.md).

WatchAnswer сначала подписывается на общий слушатель, затем читает снимок. Уведомление служит сигналом перечитать БД; накопленные изменения вычисляются относительно отправленного снимка. Потеря соединения слушателя завершает активные RPC без ложного `done`/`error`; клиент переподключается и получает свежий снимок. Слушатель восстанавливает соединение для новых подписок. Удаление пользователя будит открытые потоки: исчезнувший ответ даёт NOT_FOUND, новые запросы от tombstone блокируются TOKEN_REVOKED. GetLimit читает primary, чтобы состояние счётчика было актуальным независимо от read-after-write hint. Настройки профиля влияют на моменты будущих ответов, но не на московский сброс лимита.

## Сервер

- Keepalive: `MaxConnectionAge = 5 мин`, `MaxConnectionAgeGrace = 4 мин` (больше 3-минутного потока ответа), клиентский ping не чаще 20 с.
- Размер сообщения — `GRPC_MAX_RECV_MSG_SIZE` (4 МиБ, для `SearchOperations` — 16 МиБ).
- Готовность: раз в 5 с ping primary; результат — в gRPC health (`""`) и `/readyz`. До первого успешного ping — `NOT_SERVING`.
- Остановка по `SIGTERM`: HTTP `/readyz` (`ops.Readiness.Drain`, необратимо до конца процесса) и gRPC health одновременно перестают сообщать готовность (FIN-289) → пауза `SHUTDOWN_DELAY` (вызовы ещё принимаются, `/healthz` отвечает 200) → `GracefulStop` не дольше `SHUTDOWN_TIMEOUT` → фоновые задания → пулы БД.
- Недоступный при старте primary (отказ соединения или `connect_timeout` в DSN при живом контексте) — `go-postgres.ErrPoolUnavailable` без текста драйвера и DSN; `context.Canceled` / `DeadlineExceeded` — только когда закончился контекст вызывающего (go-postgres v0.3.0, FIN-277).

## Ошибки и совместимость (FIN-225)

Сервис использует опубликованный `contracts/gen/go v0.5.0` (`471482b1dd0d8ec556325ef5d57d5658502f8570`). `dialogue.errorWithText` одновременно заполняет `AnswerError.status` и прежние `code/text/retryable/remaining_limit`; failed-history `Message.error` получает ту же причину. User/generating/complete сообщения не получают error-payload. Оба читателя строят причину из сохранённого результата, без новой бизнес-политики в общем helper.

| Сохранённая причина | Canonical gRPC classification |
|---|---|
| MODEL_ERROR | INTERNAL (13) |
| CANNOT_ANSWER | FAILED_PRECONDITION (9) |
| OFF_TOPIC | FAILED_PRECONDITION (9) |

Это таблица сценария assistant, не ограничение общего Status. `finerr.NewResultStatus` создаёт только cause: один ErrorInfo, без RequestRetryInfo, RetryInfo, квоты или списания. `status.message` диагностический; BFF не копирует его в UI. Неизвестная сохранённая причина остаётся ошибкой данных, пока сценарий явно её не поддержит. Сервис не содержит нового UI-каталога: BFF выбирает безопасный текст и severity по canonical reason. Прежние строки, включая ASSISTANT_OFF_TOPIC_TEXT, продолжают dual-write для старых потребителей.

Внешний result.retryable разрешает **новую генерацию** через RetryAnswer: явный сохранённый false авторитетен; для старой NULL-записи остаётся существующий MODEL_ERROR fallback. В истории указатель присутствует и для false. Это не разрешение повторить исходный AskQuestion и не следствие canonical classification.

Unary advice выбирается usecase в точке отказа. `asking.RequestRetryError` сохраняет исходную ошибку через Unwrap; адаптер сначала выполняет прежний domain→finerr mapping и затем переносит явно переданное разрешение через WithRequestRetryInfo. Общий builder не вычисляет его по reason, HTTP/gRPC-коду или тексту.

| Фаза | Original-RPC advice |
|---|---|
| Оба OFF_TOPIC gate до Charge; исчерпанный дневной лимит до Charge | true после существующего ожидания RetryInfo/retry_at/resets_at |
| Отсутствие согласия, удалённый пользователь; Retry: отсутствующий ответ или отказ замены | false; существующие отдельные корректирующие сценарии сохраняются |
| Неизвестные storage/COMMIT ошибки, недоступность consent/model, shared key outcomes, replay без advice | отсутствует; новый BFF трактует отсутствие консервативно |

Разрешение сервиса относится только к исходному RPC. Оно не доказывает replay-безопасность всего REST-вызова, возраст ключа или право создать новый ключ. BFF дополнительно проверяет безопасность операции; клиент при фактической отправке соблюдает ожидание и исходный TTL. OFF_TOPIC может ждать 7 суток при обычном TTL ключа 24 часа: true не продлевает ключ и не разрешает автоматическую замену. Неопределённый исход commit и сбой cleanup не получают разрешения прежней фазы. REQUEST_IN_PROGRESS не получает true по одному наличию UUID. Bare UNAVAILABLE сохраняет старый контракт без выдуманного reason/advice.

Перед выкладкой проверить provenance всех BFF и rollback-образов: canonical декодер введён bank-bff PR10 / merge `106e5a90a2eb5d2ad07ef94661b0253c0b79471d` либо совместимым потомком. Для result.retryable минимальный поддерживаемый source-floor assistant — `3ef9e08632af3d27564bd8d4f1278478db74684a` либо потомок с сохранённой FIN-159. Отсутствие proto3 bool нельзя определять по тексту или нулевому значению. Фактический deployment этим PR не подтверждается; legacy-строки нельзя прекращать до обновления всех потребителей и их rollback floor.

Текущий опубликованный go-idempotency v0.5.0 не заменяется локальной копией. В этом теге специальный cached phone RATE_LIMITED всё ещё теряет Any/advice. Исправление сохранения Any и переоценки ожидания уже объединено в [go-idempotency PR28](https://github.com/fin-assist/go-idempotency/pull/28) (merge 5eb427), но требует НОВОГО тега, опубликованного владельцем после этого merge; v0.5.0 исправления не содержит. Потребление нового тега выполняется отдельным согласованным обновлением, без local replace. Отсутствующее разрешение в старом кеше остаётся неизвестным/консервативным: положительное advice нельзя выводить из текста, reason или самого replay. Import HTTP-сценарий вне 14 маршрутов BFF и не добавляется здесь.

## Клиенты других сервисов

`client.Dial(cfg.Peers["<name>"], client.Options{...})` (`github.com/fin-assist/go-grpc/client`; бизнес-адаптеры — `internal/adapter/client`):

- `round_robin` по всем адресам цели: в k3s сервисы headless (`dns:///<svc>.finassist.svc.cluster.local:9090`), на стенде — адрес процесса.
- Ретраи — только `UNAVAILABLE`, до 3 попыток с джиттером, только для методов из `Options.Retry`: чтения и идемпотентные по конструкции (grpc-api.md 6). Методы с `idempotency-key` ретраит приложение.
- В каждый вызов — `x-service-authorization` и `x-request-id`. Токен пользователя и подсказки клиента дальше не передаются: вызов из сервиса — системный, `user_id` — полем запроса.
- Дедлайн входящего вызова передаётся сам (контекст); вызов без дедлайна (задания, outbox) получает `DefaultTimeout`, 5 с. Потоки — без дедлайна по умолчанию, у них свои сроки.
- Keepalive клиента — 30 с.

Адаптеры `AssistantConsent` и `FinancialData`
([tools.md](tools.md)) подключены в `wire()`. Согласие читается без кеша системным
GetAssistantConsentStatus (primary по контракту user); финансовые чтения используют отдельный
серверный Subject. Каждый RPC ограничен 5 секундами, GetAnalytics — 10 секундами, с сохранением
более раннего deadline. Лимиты результата и коллекций задаются явно; непригодный ответ соседа
не становится достоверным пустым результатом. Решение о списании принимает сценарий по доверенному Evidence.

## Транзакции и чтение

`postgres.DB` делегирует lifecycle пулов primary/replica и транзакции библиотеке `go-postgres v0.3.0` ([FIN-240](https://finassist.youtrack.cloud/issue/FIN-240)); [migrations.md](migrations.md), «Запросы на pgx»:

- `db.Run(ctx, func(ctx) error)` — транзакция на primary: commit при `nil`, rollback при ошибке и панике; вложенный `Run` присоединяется к внешнему.
- `db.Writer(ctx)` — транзакция контекста или primary; `db.Reader(ctx)` — транзакция, primary при `x-read-primary: 1` от `bank-bff`, иначе реплики.
- `db.Tx(ctx)` — транзакция контекста для библиотек, которые пишут в одной транзакции с эффектом (идемпотентность, outbox): без транзакции они возвращают ошибку, а не пишут мимо неё.

Контекст другого менеджера отвергается: `Run` не вызывает callback, `db.Tx` возвращает `false`, `Writer`/`Reader` возвращают ошибку без запроса в чужую транзакцию или autocommit в своём пуле. Только внешний вызов завершает транзакцию; откат после отмены запроса ограничен 5 секундами. Контекст транзакции нельзя сохранять после callback или использовать конкурентно.

`db.RunMapped(ctx, opts, mapper, fn)` обрабатывает ожидаемые FK/UNIQUE как при запросе, так и на настоящем COMMIT. Соответствия SQLSTATE + constraint → доменная ошибка определяет адаптер конкретной операции; вложенный ненулевой mapper отвергается. Сейчас assistant не использует такие соответствия: неизвестные нарушения остаются внутренними ошибками и не раскрывают PostgreSQL-текст в RPC. Новые доменные ошибки библиотека не вводит.

Чтение диалога использует `RunWith` на primary с REPEATABLE READ READ ONLY. Снаружи это отдельный согласованный снимок; внутри транзакции своего менеджера чтение присоединяется к ней, видит её незакоммиченные записи и сохраняет её опции. Текущие RPC чтения вызывают его вне транзакции; на сетевые запросы за версиями данных это правило не распространяет границы транзакции.

## Наблюдаемость

- **Логи** — `slog`, JSON (text в dev), в каждой строке `service`, `request_id`, `caller`, `user_id` из контекста (методы `*Context`). Строка на вызов: `method`, `code`, `reason`, `duration_ms`; произвольные тексты ошибок и payload паник не записываются. На `debug` — metadata и запрос: `authorization`, `x-service-authorization`, `idempotency-key`, ключи и поля с `token` в имени маскируются, поля `bytes` (файл выписки, ключ) очищаются независимо от имени.
- **Метрики** (`/metrics` на `HTTP_ADDR`): `grpc_server_handled_total{method,code,reason}`, `grpc_server_handling_seconds{method}`, `finassist_grpc_denied_total{method,caller,cause}`, `grpc_client_handled_total{peer,method,code}`, `grpc_client_handling_seconds{peer,method}`, `finassist_identity_rejected_total{kind,reason}`, `finassist_identity_jwks_skipped_keys{source}`, `finassist_tombstone_revoked_total` без меток, метрики Go и процесса. **Без `user_id` и других неограниченных значений в метках.**
- **Проверки**: `/healthz` — процесс жив, `/readyz` — БД отвечает; то же в gRPC health. Работает и под systemd без проб Kubernetes.

## Фоновые задания

`github.com/fin-assist/go-worker`: `worker.Run(ctx, worker.Options{Logger: log, Clock: worker.SystemClock{}}, jobs...)` — задание с ошибкой или паникой перезапускается через 5 с; `worker.Every(name, interval, log, worker.SystemClock{}, fn)` — периодическое. Задания работают в каждом поде; единственность — блокировкой или `FOR UPDATE SKIP LOCKED`, как в outbox. Задания возвращает `wire()`.

## Транспорт AI Studio (FIN-220)

`internal/adapter/aistudio` подключён к рабочему циклу: один POST без автоматического повтора, store=false, обязательный x-data-logging-enabled=false, явные байтовые границы и deadline. Протокол, безопасность, тесты и ограничения проверки — [aistudio.md](aistudio.md).

Метрики генерации: `finassist_assistant_questions_accepted_total`, `finassist_assistant_generations_total{outcome,charged}`, `finassist_assistant_generation_seconds{outcome}`, `finassist_assistant_function_calls_total{function}`, `finassist_assistant_search_calls_total`, `finassist_assistant_model_tokens_total{kind}`, `finassist_assistant_refunds_total`, `finassist_assistant_off_topic_daily_count`; библиотека ключей добавляет `finassist_idempotency_requests_total{endpoint,outcome}`. Метки ограничены фиксированными именами, неизвестное сворачивается в unknown. Usage-счётчики раздельны и не складываются в цену; это наблюдения процесса, не финансовый журнал. Задания: stale-generations каждые 5 секунд, idempotency-cleanup каждую минуту, батчи ограничены общим дедлайном 5 секунд.

### Наблюдение OFF_TOPIC (FIN-161)

Долю терминальных исходов OFF_TOPIC считают по `sum(rate(finassist_assistant_generations_total{outcome="OFF_TOPIC"}[10m])) / sum(rate(finassist_assistant_generations_total[10m]))`; при пустом знаменателе данных нет. Это доля завершённых генераций.

`finassist_assistant_off_topic_daily_count` — гистограмма накопленного за московские сутки числа OFF_TOPIC пользователя после каждого нового терминального OFF_TOPIC (buckets 1,2,3,4,5,8,16,32). Сэмплы имеют вес событий: это не итоговая равновзвешенная статистика уникальных пользователей/суток. Сброс недельной защиты не обнуляет дневное наблюдение. После rollback, повторной финализации и чтения истории новых сэмплов нет. user_id и даты в labels отсутствуют. Как и прочие процессные метрики, сбой между commit и наблюдением может потерять сэмпл; допуск использует только primary PostgreSQL.

Для алерта на рост доли нужны сравнение доли за 10 минут с базовой за сутки, минимальное число завершений и выдержка, чтобы единичный вопрос не создавал шум. Порог, маршрут и правила Prometheus применяет инфраструктура после выбора рабочих значений ([FIN-228](https://finassist.youtrack.cloud/issue/FIN-228)); наличие счётчика и этого описания не означает включённый алерт. Встроенный web_search мог завершиться у провайдера до доставки off_topic: его стоимость не отменяется, фактические поиски остаются в метрике. Последующих вызовов после терминального сигнала нет.
