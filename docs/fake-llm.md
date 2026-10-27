# Локальная модель и записи Responses (FIN-142)

`internal/fakellm` — сценарный HTTP-сервер, подменяющий только AI Studio.
В тесте достаточно `handler, err := fakellm.Default()` и `httptest.NewServer(handler)`:
настоящий адаптер получает `server.URL + "/v1/responses"`. Сервис сам проверяет согласие,
лимит, исполняет функции через gRPC, строит блоки и сохраняет поток в PostgreSQL.
Фейк не считает финансовые показатели и не выполняет веб-поиск.

## Запуск на компьютере

```sh
go run ./cmd/fake-llm -addr 127.0.0.1:8092
# Свой набор вместо встроенного:
go run ./cmd/fake-llm -scenarios ./my-scenarios.json
```

Для `assistant` нужны `ENV=dev`, `AUTH_MODE=insecure-dev`,
`ASSISTANT_MODEL_URI=gpt://local/aliceai-llm-flash`, `ASSISTANT_MODEL_FOLDER_ID=local`,
`ASSISTANT_MODEL_ENDPOINT=http://127.0.0.1:8092/v1/responses` и
`ASSISTANT_MODEL_API_KEY_FILE` с любым непустым синтетическим ключом.
Параметры соседей `PEER_USER_ADDR`, `PEER_BANK_STATEMENT_DATA_ADDR`, `PEER_ANALYTICS_ADDR`
указывают на реальные локальные сервисы; согласие и импорт данных настраиваются там.

В compose фейк делит сетевое пространство `assistant`, поэтому его адрес остаётся loopback:

```sh
# Сначала задайте три PEER_*_ADDR, доступные из контейнера assistant.
docker compose -f compose.yaml -f compose.fake-llm.yaml up --build
```

Для сервисов на хосте Docker Desktop можно использовать `dns:///host.docker.internal:<port>`.
Фейк слушает только loopback IP, не имеет отдельного опубликованного порта и не попадает
в основной Dockerfile. Отдельный stage unit описан ниже. Пока фейк ещё запускается,
дождитесь его готовности перед первым вопросом. `prod` отклоняет любой неофициальный адрес модели и в конфигурации, и в конструкторе HTTP-клиента. `stage` разрешает HTTP только на числовой loopback IP.

## Stage под systemd (FIN-149)

На одном stage-хосте фейк заменяет только внешнюю модель. `assistant` остаётся в `ENV=stage` с `AUTH_MODE=verify`, подписанными сервисными JWT и проверкой настоящих user JWT из `user.GetSigningKeys`. `insecure-dev`, dev-токены и dev transport overrides в stage по-прежнему запрещены. Все пять сервисов, PostgreSQL и финансовые данные остаются настоящими; для приёмки используются синтетические пользователи и выписки.

Подготовлены два необязательных файла: `deploy/systemd/fake-llm.service` и `deploy/systemd/assistant.service.d/fake-llm.conf`. Установите SHA-привязанный бинарь в `/opt/finassist/fake-llm/fake-llm`, unit в `/etc/systemd/system/fake-llm.service`, а drop-in — в `/etc/systemd/system/assistant.service.d/fake-llm.conf`. Базовый `assistant.service` сохраняется. Фейк слушает только `127.0.0.1:8092`, работает отдельным DynamicUser с лимитами памяти/CPU и без секретов; порт не публикуется nginx. У фейка нет доступных извне административных методов и истории диалогов.

Drop-in задаёт `ASSISTANT_MODEL_URI=gpt://local/aliceai-llm-flash`, `ASSISTANT_MODEL_FOLDER_ID=local` и `ASSISTANT_MODEL_ENDPOINT=http://127.0.0.1:8092/v1/responses`. Создайте `/etc/finassist/assistant/fake-model-key` с любым непустым синтетическим ключом (root, `0600`): через `LoadCredential` он попадает в `%d/fake-model-key`. Реальный API/IAM-ключ для фейка не нужен. Блок настоящей модели в базовом unit оставьте закомментированным, а model-переменные из env-файла уберите: EnvironmentFile перекрывает Environment. Адреса `PEER_USER_ADDR`, `PEER_BANK_STATEMENT_DATA_ADDR`, `PEER_ANALYTICS_ADDR` и настройки stage/user identity по-прежнему задаются в env-файле.

После установки файлов выполните `systemctl daemon-reload`, запустите `fake-llm.service` и перезапустите `assistant.service`; drop-in задаёт Wants/After для фейка. Его отказ или остановка не отключает весь assistant: история, лимит, синхронизация пояса и PurgeUserData остаются доступны, а шаг модели завершается ошибкой транспорта. Type=exec подтверждает запуск процесса, поэтому до первого приёмочного вопроса отдельно дождитесь TCP listener `127.0.0.1:8092`; порядок units сам по себе не доказывает готовность HTTP. Замена синтетического ключа применяется после рестарта assistant, как у прочих LoadCredential.

Оба слоя валидации допускают stage HTTP только на числовой loopback IP, включая IPv6 `::1`; `localhost` остаётся только для dev, чтобы stage не зависел от DNS/hosts. Credentials в URL, query, fragment, кодированный/другой путь, HTTPS на локальный фейк и внешние HTTP-адреса отвергаются. Redirect и proxy отключены. Чтобы отключить фейк, удалите drop-in, выполните daemon-reload и верните согласованную конфигурацию модели перед рестартом assistant; без model URI новые вопросы получают UNAVAILABLE, история и лимит доступны.

Файлы и тесты не подтверждают развёрнутый VPS, фактическую совместную выкладку или сквозную приёмку FIN-362. На 27.10.2026 это подготовка артефактов конфигурации.

## Сценарии

Встроенный файл `internal/fakellm/scenarios/default.json` содержит упорядоченный список:
первый совпавший regex по последнему вопросу выбирает сценарий; неизвестный вопрос
попадает в единственный `default`. Вопросы: `rows`/«расходы», `analytics`/«аналитика»,
`search`/«поиск», `model-error`/«ошибка модели», `refusal`/«не могу ответить»,
`disconnect`/«обрыв», `timeout`/«таймаут», `off-topic`/«погода», `repeat`/«повтор функций».

Каждый шаг задаёт `output`, `usage`, задержку начала `delay_ms`, задержку SSE-событий
`chunk_delay_ms`, `http_status` или `fault` (`timeout`, `disconnect`, `refusal`).
`output` поддерживает `function_call` с JSON-объектом `arguments`, `web_search_call`
и `message` с массивом текстовых фрагментов и provider `annotations`.
Последовательные вызовы — разные шаги, параллельные — несколько функций одного шага.
Ограничения самого assistant (шаги, функции, параллелизм, общий срок) действуют по-настоящему.

Текстовые фрагменты — Go templates: `.Question`, `.Results.<call_id>` и функция `json`.
`call_id` в файле — логический alias. На wire фейк, как AI Studio, возвращает имя функции в `call_id` и отдельный уникальный `id`. Адаптер нормализует корреляцию к `id`; фейк сопоставляет эти ID обратно aliases для шаблонов. Аргументы `result_id`/`coverage_result_id` переписываются на соответствующий уникальный ID.
Результаты функций читаются из настоящих `function_call_output`, числа не проходят
через float64. Например, `{{json .Results.totals}}` отражает возвращённые сервисом данные.
В production такой сценарий не используется. `present_result` ссылается на предыдущий
вызов, а строки/графики/чипы строит настоящий presenter, не фейк.

Шаг выбирается по ID предыдущего output в контексте после последнего user item.
Общего счётчика, истории на сервере и разделения по Authorization нет: одновременные
диалоги и повтор запроса не смешиваются. Перед продолжением проверяется наличие
результатов всех функций предыдущих шагов. `max_tool_calls` ограничивает только
встроенный поиск, функции им не обрезаются. При отсутствии инструмента поиска фейк
его не вызывает; `tool_tokens` возвращается в `usage.input_tokens_details`.

`max_tool_calls`, `instructions_contain`, `without_tools` в шаге — дополнительные
проверки входа. В поисковом сценарии проверяются остатки 3 → 1 → 0, инструкция о
недоступности поиска и отсутствие `web_search` в последнем запросе. Без
`x-data-logging-enabled: false`, `stream:true`, `store:false` запрос отвергается.
Инструменты вместе с structured output также дают 400. HTTP errors, refusal, обрыв
после частичного ответа и таймаут проверяют настоящий путь возврата попытки.

## Реальный AI Studio: запись и повтор

`internal/adapter/aistudio/testdata/recorded/` содержит двенадцать реальных SSE-ответов
Alice AI LLM Flash от 26.10.2026: текст, две функции в одном шаге, следующий вызов
по результатам функций, итоговый текст и поиск с `url_citation`. Вопросы и результаты
функций синтетические, ID каталога заменён на `folder`; остальной SSE сохранён.
`manifest.json` содержит время, типы output и фактический usage. Ключи и заголовок
Authorization не записываются. Две записи repeat подтверждают повтор provider call_id в одном и в следующем шаге. Ещё три correlation подтверждают приём ручного контекста с уникальными ID; schema enum в probe заранее ограничивает result_id/marker единственными значениями. Это не доказывает самостоятельный семантический выбор модели. Две unconstrained-записи сохраняют отрицательный результат без enum: модель придумала несуществующий result_id=call-b. Аргументы модели остаются недоверенными; адаптер не чинит придуманные ссылки. Гарантия правильного выбора одного из нескольких существующих результатов этими probes не устанавливается. Offline-тест повторяет всю цепочку через GenerationModel и сравнивает построенные запросы с теми, что принял live API. Записи проходят через настоящий HTTP-адаптер в
`TestIntegrationRecordedAIStudioResponses`, без доступа к интернету и оплаты.

Сверка выявила два уточнения протокола: одинаковые boolean-поля `valid` могут
дублироваться в provider metadata, а `tool_tokens` находится в `input_tokens_details`.
Транспорт допускает только одинаковые boolean-дубли `valid` в ответе; дубли остальных
полей и конфликтующие счётчики остаются ошибкой. Входящий JSON запроса проверяется
строго. Верхнеуровневый `tool_tokens` по-прежнему читается для совместимости.

Повтор записи — только явный запуск с доступом и согласованным бюджетом:

```sh
python3 scripts/record-aistudio.py --folder <folder-id> \
  --key-file /path/outside/repository/key --output /tmp/new-recording \
  --max-requests 8 --allow-paid-requests
# Только поиск с одним встроенным обращением и сроком чтения 180 секунд:
python3 scripts/record-aistudio.py --folder <folder-id> \
  --key-file /path/outside/repository/key --output /tmp/new-search \
  --dialog search --max-requests 2 --allow-paid-requests
```

Скрипт всегда использует официальный HTTPS endpoint, `store:false` и заголовок
отключения логирования; не повторяет неудавшиеся POST автоматически. Не сохраняет
реальные данные пользователя. Неизвестный результат таймаута мог быть оплачен:
`attempts.json` фиксируется до каждого обращения. Проверяйте весь выданный набор
перед переносом в репозиторий и обновляйте записи при смене модели или API.
Это проверка протокола Flash, не всех моделей и не денежный потолок. До общего
стенда с настоящими соседями эти записи также не доказывают готовность всего продукта.

## Корреляция повторных функций

AI Studio Flash возвращает `call_id = name`, в том числе для двух вызовов одной функции
в одном ответе и для следующего вызова в другом шаге. Различать результаты этим полем
нельзя. HTTP-транспорт сохраняет исходный provider call_id и проверяет уникальность `id`
элемента. GenerationModel передаёт движку `CallID = id` и заменяет call_id в raw function
item. Следующий stateless POST содержит этот же ID в function_call и function_call_output;
это проверено настоящим API. Идентичность и исходные аргументы/данные функций не меняются.
Движок по-прежнему отклоняет повторные канонические ID; разрешения дублей в нём нет.

Сценарий repeat вызывает aggregate_operations дважды в одном шаге и ещё раз в следующем,
затем present_result трижды в одном ответе. API-тест получает разные суммы 100/200/300
и проверяет, что сервер показал каждую в нужном блоке, без перезаписи или перестановки.
Записи `unconstrained-*` сняты однократно прежней версией recorder без enum и сохранены
как исторические отрицательные регрессии. Текущий recorder их не воспроизводит и не
перезаписывает. При обновлении положительных записей эти fixtures сохраняются отдельно:
новый запуск модели не обязан повторить именно ошибку `call-b`. Если состав корпуса
меняется, вместе с обоснованием обновляются manifest и ожидаемые записи replay-теста.

Для повторной сверки доступны `--dialog repeat` и `--dialog correlation --normalize-call-ids`.
Эти probes запускаются только явно и не входят в стандартную тройку диалогов recorder.
