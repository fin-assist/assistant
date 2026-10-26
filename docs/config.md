# Конфигурация

Всё — переменные окружения (`internal/config`). Секрет можно передать файлом: `VAR_FILE=/путь` вместо `VAR` (systemd `LoadCredential`, смонтированный Secret Kubernetes); заданы оба — ошибка запуска. Ошибки конфигурации выводятся все сразу и не содержат значений секретов.

Адреса других сервисов и DSN задаются обычными настройками. В первом этапе FIN-220 режим verify использует Kubernetes ServiceAccount/JWKS; отдельная схема подписанных файловых токенов systemd-стенда пока не подключена.

Текстовый рабочий цикл подключён. По умолчанию `ASSISTANT_MODEL_URI` пуст: новые AskQuestion/RetryAnswer возвращают UNAVAILABLE без списания. Для работы нужны вместе URI модели Alice LLM/Flash, ID того же каталога, `ASSISTANT_MODEL_API_KEY_FILE` и peer-адреса user, bank-statement-data, analytics. Transport перечитывает файл перед каждым шагом; конфигурация не читает содержимое ключа и не принимает его значением переменной. Частичная настройка каталога или файла без URI — ошибка запуска. Чтение истории и лимита не требует модели. Готовность компонентов и применение квот — в [generation.md](generation.md). На стенде (`deploy/systemd/assistant.service`, FIN-349) модель включается по желанию: ключ — статический API-ключ AI Studio (не IAM-токен) в credential `model-api-key` из `/etc/finassist/assistant/model-api-key` (0600, владелец root). `LoadCredential=` копирует файл один раз при старте unit, поэтому замена ключа применяется только после `systemctl restart assistant`, а IAM-токен (живёт не больше 12 часов) протух бы до перезапуска. Блок `LoadCredential=`/`ASSISTANT_MODEL_API_KEY_FILE=%d/model-api-key` в unit закомментирован, потому что файл ключа без URI — ошибка запуска, а `LoadCredential=` отсутствующего файла не даёт стартовать unit; URI, ID каталога и peer-адреса bank-statement-data и analytics задаются в env-файле, и только вместе с раскомментированным блоком.

Входные бюджеты 24 000 токенов на шаг, 48 000 на вопрос и 16 000 для полных пар истории — расчётные. Пределы JSON-запросов 65 536 и 131 072 байта проверяются строго. Все квоты положительны; бюджеты шага не превышают суммарные, история — вход шага, а параллельные функции — общий предел функций. Общий deadline генерации — фиксированные контрактом три минуты; переменной для его изменения нет.

## Как меняется

Таблицы ниже **генерируются** из списка `Vars` в `internal/config/vars.go` — там у каждой переменной имя, значение по умолчанию, признак секрета и описание; код берёт значения по умолчанию оттуда же. Новая переменная или новое значение по умолчанию:

1. строка в `Vars`, чтение в `load` / `loadMigrate`;
2. `go test ./internal/config -run TestUnitDocIsCurrent -update` — переписывает таблицы между маркерами.

Руками блоки между маркерами не правятся. CI (`go test`) падает, если таблица устарела (`TestUnitDocIsCurrent`), если код читает необъявленную переменную или объявленная нигде не читается (`TestUnitVarsMatchWhatLoadReads`).

## `serve`

<!-- generated:config-serve:start (go test ./internal/config -run TestUnitDocIsCurrent -update) -->
| Переменная | По умолчанию | Что |
|---|---|---|
| `SERVICE_NAME` | — (обязательна) | имя сервиса: логи, `application_name`, токен в dev |
| `ENV` | `prod` | `dev`, `stage` или `prod`. Reflection включён только при `dev` |
| `AUTH_MODE` | `verify` | `verify` — go-identity `NewAssembly`: JWT пользователя по ключам user, сервисные токены — ServiceAccount через JWKS Kubernetes при `ENV=prod` или JWT стенда по файлам ключей при `ENV=stage`; при `ENV=dev` не допускается. `insecure-dev` — токены `dev:<имя>` без проверки, только при `ENV=dev` |
| `ASSISTANT_DAILY_MAX` | `5` | суточный лимит вопросов; 1–32767 по типу счётчика БД, сброс в полночь по Москве |
| `ASSISTANT_OFF_TOPIC_MAX` | `5` | число бесплатных вопросов не по теме за скользящее окно; 1–32767 |
| `ASSISTANT_OFF_TOPIC_WINDOW` | `168h` | скользящее окно вопросов не по теме; положительное, с точностью до целых миллисекунд |
| `ASSISTANT_OFF_TOPIC_TEXT` | `Вопрос не по теме — помощник отвечает только на вопросы о ваших финансах. С другими вопросами помогут универсальные ассистенты: Алиса AI, GigaChat, ChatGPT, Claude, DeepSeek` | серверный текст ответа OFF_TOPIC в потоке и истории, в том числе для ранее сохранённых ответов; непустой UTF-8, не больше 4096 байт |
| `ASSISTANT_MODEL_URI` | — | `gpt://<folder>/aliceai-llm` или `aliceai-llm-flash` в том же каталоге; пусто — генерация не настроена, чтение истории и лимита доступно |
| `ASSISTANT_MODEL_FOLDER_ID` | — | ID каталога AI Studio; обязателен вместе с URI модели и файлом ключа |
| `ASSISTANT_MODEL_ENDPOINT` | `https://ai.api.cloud.yandex.net/v1/responses` | точный официальный Responses endpoint; только в dev допускается HTTP loopback с путём `/v1/responses`; credentials, query, fragment и кодированный путь запрещены |
| `ASSISTANT_MODEL_API_KEY_FILE` | — | путь к файлу API/IAM-ключа; обязателен с URI модели. Значение ключа через ENV не поддерживается; transport читает файл перед каждым шагом, учитывая ротацию |
| `ASSISTANT_INPUT_TOKENS_PER_STEP` | `24000` | расчётный бюджет отправляемого входа одного шага; положительный, не больше суммарного бюджета |
| `ASSISTANT_INPUT_TOKENS_TOTAL` | `48000` | расчётный суммарный бюджет входа всех шагов одного вопроса; повторно отправленный контекст учитывается снова |
| `ASSISTANT_HISTORY_TOKENS` | `16000` | расчётный бюджет последних полных пар истории без суммаризации; положительный, не больше входа одного шага |
| `ASSISTANT_REQUEST_BYTES_PER_STEP` | `65536` | строгий предел байтов JSON-запроса модели на шаг; положительный, не больше суммарного предела |
| `ASSISTANT_REQUEST_BYTES_TOTAL` | `131072` | строгий суммарный предел байтов запросов всех шагов вопроса |
| `ASSISTANT_MAX_OUTPUT_TOKENS` | `4096` | предел выходных токенов одного шага; минимум 16 по Responses transport |
| `ASSISTANT_MAX_STEPS` | `8` | положительный предел шагов обращения к модели на вопрос |
| `ASSISTANT_MAX_FUNCTIONS` | `16` | положительный суммарный предел вызовов функций на вопрос |
| `ASSISTANT_PARALLEL_FUNCTIONS` | `4` | число одновременных вызовов функций; положительное, не больше суммарного предела функций |
| `ASSISTANT_MAX_SEARCH_CALLS` | `3` | положительный предел встроенных вызовов поиска на вопрос |
| `ASSISTANT_FINANCIAL_RESULT_BYTES` | `32768` | положительный технический предел байтов результата финансового инструмента |
| `ASSISTANT_FINANCIAL_COLLECTION_ITEMS` | `200` | положительный технический предел элементов коллекции в результате финансового инструмента |
| `ASSISTANT_MODEL_RESPONSE_BYTES` | `1048576` | положительный технический предел байтов ответа модели за шаг; резервируется проверка одного дополнительного байта |
| `ASSISTANT_MODEL_EVENT_BYTES` | `65536` | положительный предел байтов одного SSE-события; не больше ответа модели |
| `ASSISTANT_MODEL_ARGUMENT_BYTES` | `16384` | положительный предел байтов аргументов одного вызова функции; не больше ответа модели |
| `ASSISTANT_MODEL_OUTPUT_ITEMS` | `64` | положительный технический предел элементов output одного ответа модели |
| `AUTH_USER_ISSUER` | — | доверенный issuer access JWT user; обязателен при verify |
| `AUTH_USER_AUDIENCE` | — | доверенная audience access JWT user; обязательна при verify |
| `AUTH_KUBERNETES_ISSUER` | `https://kubernetes.default.svc.cluster.local` | issuer токенов ServiceAccount; сверить с настройкой кластера |
| `AUTH_KUBERNETES_JWKS_URL` | `https://kubernetes.default.svc/openid/v1/jwks` | HTTPS-адрес ключей ServiceAccount без query, fragment и credentials |
| `AUTH_KUBERNETES_CA_FILE` | `/var/run/secrets/kubernetes.io/serviceaccount/ca.crt` | CA Kubernetes API |
| `AUTH_KUBERNETES_API_TOKEN_FILE` | `/var/run/secrets/kubernetes.io/serviceaccount/token` | API-токен для чтения JWKS; отдельный от finassist-internal |
| `AUTH_KUBERNETES_NAMESPACE` | `finassist` | доверенный namespace ServiceAccount |
| `AUTH_STAGE_ISSUER` | — | issuer сервисных JWT стенда, общий для сервисов стенда; обязателен при verify и `ENV=stage`, запрещён при `ENV=prod` |
| `AUTH_STAGE_SIGNING_KEY_FILE` | — | файл собственного PKCS#8 PEM Ed25519 ключа для исходящих JWT стенда (`kid` = `sub` = `SERVICE_NAME`); читается при старте; обязателен при verify и `ENV=stage`, запрещён при `ENV=prod` |
| `AUTH_STAGE_PUBLIC_KEYS_FILE` | — | JWKS публичных Ed25519 ключей сервисов стенда (`kid` — имя сервиса); читается при старте; обязателен при verify и `ENV=stage`, запрещён при `ENV=prod` |
| `SERVICE_TOKEN_FILE` | — | файл с projected-токеном ServiceAccount (audience `finassist-internal`) для исходящих вызовов; обязателен при verify и `ENV=prod`, в stage не используется (FIN-119) |
| `DB_PRIMARY_DSN` / `_FILE` | — (обязательна) | DSN primary (`-rw`) |
| `DB_REPLICA_DSN` / `_FILE` | — | DSN реплик (`-ro`); пусто — чтение тоже с primary. На стенде оба указывают на один PostgreSQL |
| `DB_MAX_CONNS` | `10` | размер каждого пула |
| `GRPC_ADDR` | `:9090` | gRPC |
| `GRPC_MAX_RECV_MSG_SIZE` | `4194304` | байт; 16 МиБ там, где обслуживается `SearchOperations` (grpc-api.md 9) |
| `HTTP_ADDR` | `:9091` | `/metrics`, `/healthz`, `/readyz` |
| `PEER_<NAME>_ADDR` | — | адрес другого сервиса: `PEER_BANK_STATEMENT_DATA_ADDR` → `cfg.Peers["bank-statement-data"]`. k3s: `dns:///bank-statement-data.finassist.svc.cluster.local:9090`; стенд: `dns:///127.0.0.1:9092` |
| `LOG_LEVEL` | `info` | `debug` добавляет в строку вызова metadata и запрос с замаскированными токенами |
| `LOG_FORMAT` | `json` | `json` или `text` |
| `SHUTDOWN_DELAY` | `5s` | после `NOT_SERVING` сервер ещё принимает вызовы, пока балансировщики не уберут под |
| `SHUTDOWN_TIMEOUT` | `20s` | сколько ждать завершения вызовов; оставшиеся обрываются (потоки ответа клиент переоткроет) |
<!-- generated:config-serve:end -->

## `migrate`

<!-- generated:config-migrate:start (go test ./internal/config -run TestUnitDocIsCurrent -update) -->
| Переменная | По умолчанию | Что |
|---|---|---|
| `DB_PRIMARY_DSN` / `_FILE` | — (обязательна) | DSN primary, роль сервиса. Миграции никогда не идут на реплику |
| `MIGRATE_LOCK_TIMEOUT` | `5s` | `lock_timeout`: DDL, который не дождался блокировки занятой таблицы, падает, а не держит очередь запросов сервиса за собой |
| `MIGRATE_STATEMENT_TIMEOUT` | `15m` | `statement_timeout` каждой команды |
| `MIGRATE_WAIT_TIMEOUT` | `15m` | сколько ждать, пока другой запуск миграций того же источника закончит |
<!-- generated:config-migrate:end -->

Как действуют таймауты миграций — [migrations.md](migrations.md), «Таймауты».
