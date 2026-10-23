# gRPC-сервер, клиенты, наблюдаемость

Правила межсервисного взаимодействия — в `fin-assist/contracts`, [`docs/grpc-api.md`](https://github.com/fin-assist/contracts/blob/main/docs/grpc-api.md) (разделы 3, 4, 5, 9). Здесь — как шаблон их реализует.

## Порядок интерцепторов

Каждый вызов (unary и поток) проходит по порядку:

1. **`x-request-id`** — из metadata или новый; значение неожиданной формы (не `[A-Za-z0-9._-]{1,128}`) заменяется.
2. **Логи и метрики** — после обработчика (см. «Наблюдаемость»).
3. **Перехват паники** — голый `INTERNAL`, стек в лог.
4. **Сервисный токен** `x-service-authorization` → имя вызывающего. Нет или неверный — голый `UNAUTHENTICATED`.
5. **Таблица доступа** (`AccessPolicy()` в `internal/app/wire.go`, копия таблицы grpc-api.md 3.3). Метода нет в таблице или вызывающего нет в правиле — голый `PERMISSION_DENIED`, строка в лог уровня `ERROR` и метрика `finassist_grpc_denied_total{cause="caller_not_allowed"}` (на неё — алерт). Сервер **не стартует**, если у зарегистрированного метода нет правила.
6. **Подсказки `bank-bff`** — `x-client-ip`, `x-client-version`, `x-read-primary` берутся только от `bank-bff`; от других вызывающих игнорируются.
7. **Токен пользователя** `authorization` — по модели метода:

   | Модель | `authorization` | Пользователь в обработчике |
   |---|---|---|
   | `ModelUser` | обязателен; нет — `INVALID_TOKEN` | `reqctx.UserID(ctx)` = `sub` |
   | `ModelMixed` | есть — проверяется; нет — системный вызов | `UserID` или поле `user_id` запроса |
   | `ModelSystem` | игнорируется | поле `user_id` запроса |
   | `ModelNoUser` | игнорируется | нет (вход: `AuthService`) |

   Ошибки проверки — из реестра (`INVALID_TOKEN`, `TOKEN_EXPIRED`, `TOKEN_REVOKED`), они доходят до клиента. Токен, принятый верификатором без `sub`, — тоже `INVALID_TOKEN`: пользовательский вызов не превращается в системный.
8. **Tombstone** — удалённый пользователь в вызове от его имени получает `TOKEN_REVOKED`. Сбой проверки — голый `UNAVAILABLE` (обработчик ещё не выполнялся, повтор безопасен). Системные вызовы для удалённого пользователя обрабатывает сам обработчик: OK без действий (grpc-api.md 6.1).
9. **protovalidate** — нарушения аннотаций `.proto` → `VALIDATION_ERROR` с `BadRequest` по полям; `reason` нарушения: `required` → `REQUIRED`, формат (`pattern`, `email`, `uuid`, `prefix`, `defined_only`, …) → `INVALID_FORMAT`, остальное (длины, диапазоны, размеры, CEL) → `OUT_OF_RANGE`. В потоках проверяется каждое входящее сообщение.

10. **Нормализация ошибки обработчика** — вызывающему уходит только статус: обёрнутый `fmt.Errorf("…: %w", finerr.NotFound())` разворачивается до самого статуса (текст обёртки не уходит), ошибка без статуса (`pgx`, `io`, своя) становится голым `INTERNAL`, `context.Canceled` / `DeadlineExceeded` — голыми `CANCELLED` / `DEADLINE_EXCEEDED`. Без этого grpc-go отправил бы `UNKNOWN` с `err.Error()` в `status.message`. Исходная ошибка — в строке лога вызова (`error`).

Голые коды без `ErrorInfo` (шаги 3–5, 8, 10) — внутренние отказы, `bank-bff` отдаёт по ним 500 (grpc-api.md 5.1, правило 3). Строит их только `bare()` в `internal/adapter/grpc/server/status.go`; остальные ошибки — конструкторы `finerr`, обход запрещён линтером (`forbidigo`, `.golangci.yml`).

Health (`grpc.health.v1`) и reflection (только `ENV=dev`) открыты без токенов.

## Идентичность и удаление

`AUTH_MODE=verify` использует выпущенную `go-identity v0.1.0`: `UserVerifier` получает Ed25519-ключи через authenticated `user.UserInternalService/GetSigningKeys`; `ServiceVerifier` проверяет подпись, issuer, audience `finassist-internal`, namespace и согласованность claims ServiceAccount по JWKS Kubernetes. HTTP использует cluster CA; API-токен чтения JWKS и исходящий internal-токен лежат в разных файлах. Issuer/audience access JWT задаются явно. `PEER_USER_ADDR` обязателен. Ключи обновляются фоном (user — 10 мин, cluster — 1 час); завершение приложения отменяет обновления и закрывает соединения.

`AUTH_MODE=insecure-dev` разрешён только при `ENV=dev`: `Bearer dev:bank-bff` в `x-service-authorization`, `Bearer dev:<user_id>` в `authorization`. Пользовательские ID работающих методов — UUID. Проверка dev-ограничения есть и в конфигурации, и в сборке. Подписанные файловые JWT отдельного systemd-стенда пока не подключены; запуск stage без Kubernetes-параметров проверку не обходит.

`go-tombstone v0.1.0` читает primary без кеша при admission. Удалённый пользователь получает TOKEN_REVOKED; сбой чтения — UNAVAILABLE до обработчика. Каждый путь записи берёт `Guard` в сервисной READ COMMITTED транзакции до блокировок строк. Системный получатель после удаления отвечает OK без эффекта. `PurgeUserData` удаляет `user_state` и каскадные записи и фиксирует tombstone одной транзакцией, повтор не меняет результат. Tombstone бессрочный, миграции библиотеки идут перед сервисом в отдельной таблице версий.

FIN-220 реализует все семь RPC, включая AskQuestion/RetryAnswer с текстовым циклом модели. При ненастроенной модели новые вопросы/повторы завершаются UNAVAILABLE без списания. История и снимки потока читаются с primary в REPEATABLE READ, без внешних вызовов внутри транзакции. Версии для `changed_since` запрашиваются после чтения: один GetDataVersion на страницу с источниками, в потоке — для нового источника. Сбой этого чтения завершает RPC, не создавая ошибку модели. Принятие, возвраты, отмена, бюджеты и оставшиеся этапы — в [generation.md](generation.md).

WatchAnswer сначала подписывается на общий слушатель, затем читает снимок. Уведомление служит сигналом перечитать БД; накопленные изменения вычисляются относительно отправленного снимка. Потеря соединения слушателя завершает активные RPC без ложного `done`/`error`; клиент переподключается и получает свежий снимок. Слушатель восстанавливает соединение для новых подписок. Удаление пользователя будит открытые потоки: исчезнувший ответ даёт NOT_FOUND, новые запросы от tombstone блокируются TOKEN_REVOKED. GetLimit читает primary, чтобы состояние счётчика было актуальным независимо от read-after-write hint. Настройки профиля влияют на моменты будущих ответов, но не на московский сброс лимита.

## Сервер

- Keepalive: `MaxConnectionAge = 5 мин`, `MaxConnectionAgeGrace = 4 мин` (больше 3-минутного потока ответа), клиентский ping не чаще 20 с.
- Размер сообщения — `GRPC_MAX_RECV_MSG_SIZE` (4 МиБ, для `SearchOperations` — 16 МиБ).
- Готовность: раз в 5 с ping primary; результат — в gRPC health (`""`) и `/readyz`. До первого успешного ping — `NOT_SERVING`.
- Остановка по `SIGTERM`: `NOT_SERVING` → пауза `SHUTDOWN_DELAY` → `GracefulStop` не дольше `SHUTDOWN_TIMEOUT` → фоновые задания → пулы БД.

## Клиенты других сервисов

`client.Dial(cfg.Peers["<name>"], client.Options{...})` (`internal/adapter/client`):

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

`postgres.DB` — пулы primary и реплик, [migrations.md](migrations.md), «Запросы на pgx»:

- `db.Run(ctx, func(ctx) error)` — транзакция на primary: commit при `nil`, rollback при ошибке и панике; вложенный `Run` присоединяется к внешнему.
- `db.Writer(ctx)` — транзакция контекста или primary; `db.Reader(ctx)` — транзакция, primary при `x-read-primary: 1` от `bank-bff`, иначе реплики.
- `db.Tx(ctx)` — транзакция контекста для библиотек, которые пишут в одной транзакции с эффектом (идемпотентность, outbox): без транзакции они возвращают ошибку, а не пишут мимо неё.

## Наблюдаемость

- **Логи** — `slog`, JSON (text в dev), в каждой строке `service`, `request_id`, `caller`, `user_id` из контекста (методы `*Context`). Строка на вызов: `method`, `code`, `reason`, `duration_ms`, `error` (исходная ошибка обработчика — только здесь, клиенту не уходит). На `debug` — metadata и запрос: `authorization`, `x-service-authorization`, `idempotency-key`, ключи и поля с `token` в имени маскируются, поля `bytes` (файл выписки, ключ) очищаются независимо от имени.
- **Метрики** (`/metrics` на `HTTP_ADDR`): `grpc_server_handled_total{method,code,reason}`, `grpc_server_handling_seconds{method}`, `finassist_grpc_denied_total{method,caller,cause}`, `grpc_client_handled_total{peer,method,code}`, `grpc_client_handling_seconds{peer,method}`, `finassist_identity_rejected_total{kind,reason}`, `finassist_tombstone_revoked_total` без меток, метрики Go и процесса. **Без `user_id` и других неограниченных значений в метках.**
- **Проверки**: `/healthz` — процесс жив, `/readyz` — БД отвечает; то же в gRPC health. Работает и под systemd без проб Kubernetes.

## Фоновые задания

`internal/worker`: `worker.Run(ctx, log, jobs...)` — задание с ошибкой или паникой перезапускается через 5 с; `worker.Every(name, interval, log, fn)` — периодическое. Задания работают в каждом поде; единственность — блокировкой или `FOR UPDATE SKIP LOCKED`, как в outbox. Задания возвращает `wire()`.

## Транспорт AI Studio (FIN-220)

`internal/adapter/aistudio` подключён к рабочему циклу: один POST без автоматического повтора, store=false, обязательный x-data-logging-enabled=false, явные байтовые границы и deadline. Протокол, безопасность, тесты и ограничения проверки — [aistudio.md](aistudio.md).

Метрики генерации: `finassist_assistant_questions_accepted_total`, `finassist_assistant_generations_total{outcome,charged}`, `finassist_assistant_generation_seconds{outcome}`, `finassist_assistant_function_calls_total{function}`, `finassist_assistant_search_calls_total`, `finassist_assistant_model_tokens_total{kind}`, `finassist_assistant_refunds_total`; библиотека ключей добавляет `finassist_idempotency_requests_total{endpoint,outcome}`. Метки ограничены фиксированными именами, неизвестное сворачивается в unknown. Usage-счётчики раздельны и не складываются в цену; это наблюдения процесса, не финансовый журнал. Задания: stale-generations каждые 5 секунд, idempotency-cleanup каждую минуту, батчи ограничены общим дедлайном 5 секунд.
