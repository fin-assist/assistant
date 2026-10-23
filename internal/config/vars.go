package config

import (
	"fmt"
	"strings"
)

// Var declares one environment variable: its default and its description in docs/config.md.
// Every variable the binary reads is declared here; reading an undeclared one is a
// configuration error, and the tables of docs/config.md are generated from this list
// (TestDocIsCurrent), so the documentation cannot drift from the code.
type Var struct {
	Name     string
	Command  string // serve or migrate: the table of docs/config.md it belongs to
	Default  string // as it would be written in the environment; empty = no default
	Required bool
	Secret   bool   // may also come from the file named by Name_FILE
	Doc      string // Russian, Markdown; shown in docs/config.md
}

// Vars lists every variable in the order of docs/config.md.
var Vars = []Var{
	{Name: "SERVICE_NAME", Command: "serve", Required: true,
		Doc: "имя сервиса: логи, `application_name`, токен в dev"},
	{Name: "ENV", Command: "serve", Default: string(EnvProd),
		Doc: "`dev`, `stage` или `prod`. Reflection включён только при `dev`"},
	{Name: "AUTH_MODE", Command: "serve", Default: string(AuthVerify),
		Doc: "`verify` — проверка JWT через ключи user и токенов ServiceAccount через JWKS Kubernetes; `insecure-dev` — токены `dev:<имя>` без проверки, только при `ENV=dev`"},
	{Name: "ASSISTANT_DAILY_MAX", Command: "serve", Default: "5", Doc: "суточный лимит вопросов; 1–32767 по типу счётчика БД, сброс в полночь по Москве"},
	{Name: "ASSISTANT_MODEL_URI", Command: "serve", Doc: "`gpt://<folder>/aliceai-llm` или `aliceai-llm-flash` в том же каталоге; пусто — генерация не настроена, чтение истории и лимита доступно"},
	{Name: "ASSISTANT_MODEL_FOLDER_ID", Command: "serve", Doc: "ID каталога AI Studio; обязателен вместе с URI модели и файлом ключа"},
	{Name: "ASSISTANT_MODEL_ENDPOINT", Command: "serve", Default: "https://ai.api.cloud.yandex.net/v1/responses", Doc: "точный официальный Responses endpoint; только в dev допускается HTTP loopback с путём `/v1/responses`; credentials, query, fragment и кодированный путь запрещены"},
	{Name: "ASSISTANT_MODEL_API_KEY_FILE", Command: "serve", Doc: "путь к файлу API/IAM-ключа; обязателен с URI модели. Значение ключа через ENV не поддерживается; transport читает файл перед каждым шагом, учитывая ротацию"},
	{Name: "ASSISTANT_INPUT_TOKENS_PER_STEP", Command: "serve", Default: "24000", Doc: "расчётный бюджет отправляемого входа одного шага; положительный, не больше суммарного бюджета"},
	{Name: "ASSISTANT_INPUT_TOKENS_TOTAL", Command: "serve", Default: "48000", Doc: "расчётный суммарный бюджет входа всех шагов одного вопроса; повторно отправленный контекст учитывается снова"},
	{Name: "ASSISTANT_HISTORY_TOKENS", Command: "serve", Default: "16000", Doc: "расчётный бюджет последних полных пар истории без суммаризации; положительный, не больше входа одного шага"},
	{Name: "ASSISTANT_REQUEST_BYTES_PER_STEP", Command: "serve", Default: "65536", Doc: "строгий предел байтов JSON-запроса модели на шаг; положительный, не больше суммарного предела"},
	{Name: "ASSISTANT_REQUEST_BYTES_TOTAL", Command: "serve", Default: "131072", Doc: "строгий суммарный предел байтов запросов всех шагов вопроса"},
	{Name: "ASSISTANT_MAX_OUTPUT_TOKENS", Command: "serve", Default: "4096", Doc: "предел выходных токенов одного шага; минимум 16 по Responses transport"},
	{Name: "ASSISTANT_MAX_STEPS", Command: "serve", Default: "8", Doc: "положительный предел шагов обращения к модели на вопрос"},
	{Name: "ASSISTANT_MAX_FUNCTIONS", Command: "serve", Default: "16", Doc: "положительный суммарный предел вызовов функций на вопрос"},
	{Name: "ASSISTANT_PARALLEL_FUNCTIONS", Command: "serve", Default: "4", Doc: "число одновременных вызовов функций; положительное, не больше суммарного предела функций"},
	{Name: "ASSISTANT_MAX_SEARCH_CALLS", Command: "serve", Default: "3", Doc: "положительный предел встроенных вызовов поиска на вопрос"},
	{Name: "ASSISTANT_FINANCIAL_RESULT_BYTES", Command: "serve", Default: "32768", Doc: "положительный технический предел байтов результата финансового инструмента"},
	{Name: "ASSISTANT_FINANCIAL_COLLECTION_ITEMS", Command: "serve", Default: "200", Doc: "положительный технический предел элементов коллекции в результате финансового инструмента"},
	{Name: "ASSISTANT_MODEL_RESPONSE_BYTES", Command: "serve", Default: "1048576", Doc: "положительный технический предел байтов ответа модели за шаг; резервируется проверка одного дополнительного байта"},
	{Name: "ASSISTANT_MODEL_EVENT_BYTES", Command: "serve", Default: "65536", Doc: "положительный предел байтов одного SSE-события; не больше ответа модели"},
	{Name: "ASSISTANT_MODEL_ARGUMENT_BYTES", Command: "serve", Default: "16384", Doc: "положительный предел байтов аргументов одного вызова функции; не больше ответа модели"},
	{Name: "ASSISTANT_MODEL_OUTPUT_ITEMS", Command: "serve", Default: "64", Doc: "положительный технический предел элементов output одного ответа модели"},
	{Name: "AUTH_USER_ISSUER", Command: "serve", Doc: "доверенный issuer access JWT user; обязателен при verify"},
	{Name: "AUTH_USER_AUDIENCE", Command: "serve", Doc: "доверенная audience access JWT user; обязательна при verify"},
	{Name: "AUTH_KUBERNETES_ISSUER", Command: "serve", Default: "https://kubernetes.default.svc.cluster.local", Doc: "issuer токенов ServiceAccount; сверить с настройкой кластера"},
	{Name: "AUTH_KUBERNETES_JWKS_URL", Command: "serve", Default: "https://kubernetes.default.svc/openid/v1/jwks", Doc: "HTTPS-адрес ключей ServiceAccount без query, fragment и credentials"},
	{Name: "AUTH_KUBERNETES_CA_FILE", Command: "serve", Default: "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt", Doc: "CA Kubernetes API"},
	{Name: "AUTH_KUBERNETES_API_TOKEN_FILE", Command: "serve", Default: "/var/run/secrets/kubernetes.io/serviceaccount/token", Doc: "API-токен для чтения JWKS; отдельный от finassist-internal"},
	{Name: "AUTH_KUBERNETES_NAMESPACE", Command: "serve", Default: "finassist", Doc: "доверенный namespace ServiceAccount"},

	{Name: "SERVICE_TOKEN_FILE", Command: "serve",
		Doc: "файл с токеном ServiceAccount этого сервиса для исходящих вызовов (FIN-119)"},
	{Name: "DB_PRIMARY_DSN", Command: "serve", Required: true, Secret: true,
		Doc: "DSN primary (`-rw`)"},
	{Name: "DB_REPLICA_DSN", Command: "serve", Secret: true,
		Doc: "DSN реплик (`-ro`); пусто — чтение тоже с primary. На стенде оба указывают на один PostgreSQL"},
	{Name: "DB_MAX_CONNS", Command: "serve", Default: "10",
		Doc: "размер каждого пула"},
	{Name: "GRPC_ADDR", Command: "serve", Default: ":9090",
		Doc: "gRPC"},
	{Name: "GRPC_MAX_RECV_MSG_SIZE", Command: "serve", Default: "4194304",
		Doc: "байт; 16 МиБ там, где обслуживается `SearchOperations` (grpc-api.md 9)"},
	{Name: "HTTP_ADDR", Command: "serve", Default: ":9091",
		Doc: "`/metrics`, `/healthz`, `/readyz`"},
	{Name: "PEER_<NAME>_ADDR", Command: "serve",
		Doc: "адрес другого сервиса: `PEER_BANK_STATEMENT_DATA_ADDR` → `cfg.Peers[\"bank-statement-data\"]`. k3s: `dns:///bank-statement-data.finassist.svc.cluster.local:9090`; стенд: `dns:///127.0.0.1:9092`"},
	{Name: "LOG_LEVEL", Command: "serve", Default: "info",
		Doc: "`debug` добавляет в строку вызова metadata и запрос с замаскированными токенами"},
	{Name: "LOG_FORMAT", Command: "serve", Default: "json",
		Doc: "`json` или `text`"},
	{Name: "SHUTDOWN_DELAY", Command: "serve", Default: "5s",
		Doc: "после `NOT_SERVING` сервер ещё принимает вызовы, пока балансировщики не уберут под"},
	{Name: "SHUTDOWN_TIMEOUT", Command: "serve", Default: "20s",
		Doc: "сколько ждать завершения вызовов; оставшиеся обрываются (потоки ответа клиент переоткроет)"},

	{Name: "DB_PRIMARY_DSN", Command: "migrate", Required: true, Secret: true,
		Doc: "DSN primary, роль сервиса. Миграции никогда не идут на реплику"},
	{Name: "MIGRATE_LOCK_TIMEOUT", Command: "migrate", Default: "5s",
		Doc: "`lock_timeout`: DDL, который не дождался блокировки занятой таблицы, падает, а не держит очередь запросов сервиса за собой"},
	{Name: "MIGRATE_STATEMENT_TIMEOUT", Command: "migrate", Default: "15m",
		Doc: "`statement_timeout` каждой команды"},
	{Name: "MIGRATE_WAIT_TIMEOUT", Command: "migrate", Default: "15m",
		Doc: "сколько ждать, пока другой запуск миграций того же источника закончит"},
}

// Table renders the Markdown table of one command for docs/config.md.
func Table(command string) string {
	var b strings.Builder
	b.WriteString("| Переменная | По умолчанию | Что |\n|---|---|---|\n")
	for _, v := range Vars {
		if v.Command != command {
			continue
		}
		name := "`" + v.Name + "`"
		if v.Secret {
			name += " / `_FILE`"
		}
		def := "—"
		switch {
		case v.Default != "":
			def = "`" + v.Default + "`"
		case v.Required:
			def = "— (обязательна)"
		}
		fmt.Fprintf(&b, "| %s | %s | %s |\n", name, def, v.Doc)
	}
	return b.String()
}
