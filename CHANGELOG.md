# История изменений

## [Unreleased]

- FIN-256: общие server/client/reqctx подключены из `go-grpc v0.1.0`, миграции и тестовый harness — из опубликованного `go-migrate v0.1.1` с исправлением library up/down/up (FIN-262), фоновые циклы — из `go-worker v0.1.0`, lifecycle пулов — из `go-postgres v0.2.0`. Добавлены HTTP/SSE integration-проверки повреждённого lifecycle и отказов провайдера: без terminal output/tool calls, утечки ошибок и повторного POST. Удалены локальные копии инфраструктуры; сохранены streaming, identity, read-primary, транзакции, idempotency, диалог и режим без модели.

### Изменено

- FIN-240: управление транзакциями перенесено на `go-postgres v0.1.1`: общий менеджер для запросов и COMMIT, отказ от контекста чужого менеджера без записи вне транзакции. Пулы primary/replica и атомарность idempotency/tombstone сохранены. Чтение диалога использует `RunWith`: отдельно — primary REPEATABLE READ READ ONLY, внутри своей транзакции — её снимок и опции. Новых ошибок контракта и изменений схемы нет.
