# Конфигурация

Всё — переменные окружения (`internal/config`). Секрет можно передать файлом: `VAR_FILE=/путь` вместо `VAR` (systemd `LoadCredential`, смонтированный Secret Kubernetes); заданы оба — ошибка запуска. Ошибки конфигурации выводятся все сразу и не содержат значений секретов.

Адреса других сервисов и DSN задаются обычными настройками. В первом этапе FIN-220 режим verify использует Kubernetes ServiceAccount/JWKS; отдельная схема подписанных файловых токенов systemd-стенда пока не подключена.

## Как меняется

Таблицы ниже **генерируются** из списка `Vars` в `internal/config/vars.go` — там у каждой переменной имя, значение по умолчанию, признак секрета и описание; код берёт значения по умолчанию оттуда же. Новая переменная или новое значение по умолчанию:

1. строка в `Vars`, чтение в `load` / `loadMigrate`;
2. `go test ./internal/config -run TestDocIsCurrent -update` — переписывает таблицы между маркерами.

Руками блоки между маркерами не правятся. CI (`go test`) падает, если таблица устарела (`TestDocIsCurrent`), если код читает необъявленную переменную или объявленная нигде не читается (`TestVarsMatchWhatLoadReads`).

## `serve`

<!-- generated:config-serve:start (go test ./internal/config -run TestDocIsCurrent -update) -->
| Переменная | По умолчанию | Что |
|---|---|---|
| `SERVICE_NAME` | — (обязательна) | имя сервиса: логи, `application_name`, токен в dev |
| `ENV` | `prod` | `dev`, `stage` или `prod`. Reflection включён только при `dev` |
| `AUTH_MODE` | `verify` | `verify` — проверка JWT через ключи user и токенов ServiceAccount через JWKS Kubernetes; `insecure-dev` — токены `dev:<имя>` без проверки, только при `ENV=dev` |
| `ASSISTANT_DAILY_MAX` | `5` | суточный лимит вопросов; 1–32767 по типу счётчика БД, сброс в полночь по Москве |
| `AUTH_USER_ISSUER` | — | доверенный issuer access JWT user; обязателен при verify |
| `AUTH_USER_AUDIENCE` | — | доверенная audience access JWT user; обязательна при verify |
| `AUTH_KUBERNETES_ISSUER` | `https://kubernetes.default.svc.cluster.local` | issuer токенов ServiceAccount; сверить с настройкой кластера |
| `AUTH_KUBERNETES_JWKS_URL` | `https://kubernetes.default.svc/openid/v1/jwks` | HTTPS-адрес ключей ServiceAccount без query, fragment и credentials |
| `AUTH_KUBERNETES_CA_FILE` | `/var/run/secrets/kubernetes.io/serviceaccount/ca.crt` | CA Kubernetes API |
| `AUTH_KUBERNETES_API_TOKEN_FILE` | `/var/run/secrets/kubernetes.io/serviceaccount/token` | API-токен для чтения JWKS; отдельный от finassist-internal |
| `AUTH_KUBERNETES_NAMESPACE` | `finassist` | доверенный namespace ServiceAccount |
| `SERVICE_TOKEN_FILE` | — | файл с токеном ServiceAccount этого сервиса для исходящих вызовов (FIN-119) |
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

<!-- generated:config-migrate:start (go test ./internal/config -run TestDocIsCurrent -update) -->
| Переменная | По умолчанию | Что |
|---|---|---|
| `DB_PRIMARY_DSN` / `_FILE` | — (обязательна) | DSN primary, роль сервиса. Миграции никогда не идут на реплику |
| `MIGRATE_LOCK_TIMEOUT` | `5s` | `lock_timeout`: DDL, который не дождался блокировки занятой таблицы, падает, а не держит очередь запросов сервиса за собой |
| `MIGRATE_STATEMENT_TIMEOUT` | `15m` | `statement_timeout` каждой команды |
| `MIGRATE_WAIT_TIMEOUT` | `15m` | сколько ждать, пока другой запуск миграций того же источника закончит |
<!-- generated:config-migrate:end -->

Как действуют таймауты миграций — [migrations.md](migrations.md), «Таймауты».
