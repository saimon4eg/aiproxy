# Архитектура

Техническое описание aiproxy: поток запроса, пакеты, роутинг, конвертеры, журнал и инварианты.

## Границы: что наше, а что upstream

aiproxy — форк [copilot2api](https://github.com/whtsky/copilot2api) (база v0.4.0, отдельные
изменения перенесены из более поздних upstream-релизов). Разделение простое:

| Слой | Происхождение |
|---|---|
| Copilot-транспорт: `auth/`, `proxy/`, `internal/copilot`, `internal/models`, `internal/upstream`, `gemini/`, `amplocal/`, `ampsearch/` | copilot2api (поведение идентично upstream) |
| Мульти-провайдерный роутер: `providers/`, `providers/adapters/`, `providers/openai/` | наш слой |
| Конвертеры форматов: `anthropic/` | наш слой |
| Журнал и статистика: `requestlog/`, `admin.go` | наш слой |
| Провайдер `claude-cli`: `providers/claudecode*.go`, `providers/inertmcp.go` | наш слой |
| Middleware, роутинг эндпоинтов, точка входа: `main.go`, `middleware.go`, `ratelimit.go`, `debug_middleware.go` | наш слой поверх upstream |

Copilot-специфичные тонкости (заголовки Copilot API, device flow GitHub, AmpCode-эндпоинты,
Gemini-совместимый каталог) документированы в upstream-репозитории; здесь они описываются
только как часть общего потока.

## Поток запроса

```
клиент
  │
  ▼
requestIDMiddleware        X-Request-ID, создание Record, захват ответа (самый внешний слой)
  ▼
corsMiddleware             CORS-заголовки, OPTIONS → 204
  ▼
rateLimitMiddleware        100 запросов/сек на IP, /health исключён
  ▼
debugLogMiddleware         отладочный лог запроса
  ▼
latencyMiddleware          одна строка slog на запрос
  ▼
logAllRequests             отладочный лог пути
  ▼
http.ServeMux
  ├─ /v1/models            providers.ModelsCache
  ├─ /v1/messages          providers.Config.ServeHTTP
  ├─ /v1/responses         providers.Config.ServeHTTP
  ├─ /v1/chat/completions  providers.Config.ServeHTTP
  ├─ /v1/embeddings        proxy.Handler (Copilot)
  ├─ /v1beta/models[/…]    gemini.Handler (Copilot)
  ├─ /usage                proxy.Handler.HandleUsage (квота Copilot)
  ├─ /admin/stats          сводка из индекса журнала
  ├─ /admin/usage          токены по ключам и моделям
  ├─ /health               {"status":"ok","providers":N}
  └─ /amp/*, /api/*, /news.rss  AmpCode-совместимость (Copilot)
```

`requestIDMiddleware` стоит снаружи намеренно: отказ, который формирует внутренний слой
(CORS-preflight, `429` rate-limit), обязан попасть в журнал и в статистику ошибок.

Дальше для трёх инференс-эндпоинтов работает общий роутер.

## Роутинг (`providers.Config.Route`)

```
1. прочитать тело, достать model
2. алиасы Copilot (только /v1/responses, /v1/chat/completions)
3. model ~ ^copilot-  → capability-роутинг Copilot
4. иначе FindProvider по model_prefix + "-"
5. срезать префикс провайдера (кроме claude-cli)
6. выбрать обработчик по (эндпоинт, type)
```

### Copilot (`copilot-*`)

Префикс срезается, display-суффиксы (`[1m]`) убираются, неподдерживаемые Copilot поля
вычищаются. Дальше эндпоинт и **нативные возможности модели** из `/models` решают, кто отвечает:

| Эндпоинт | Модель умеет нативно | Обработчик |
|---|---|---|
| `/v1/messages` | Messages (Claude) | `copilotHandler` (passthrough) |
| `/v1/messages` | Responses (GPT) | `copilotAnthropicHandler` (Anthropic → Responses) |
| `/v1/responses` | Responses (GPT) | `copilotHandler` (passthrough) |
| `/v1/responses` | Messages (Claude) | `makeCopilotResponsesToMessagesHandler` (Responses → Anthropic) |
| `/v1/chat/completions` | — | `copilotHandler` |

При холодном кэше моделей работает эвристика: `gpt-*` — Responses, остальные — Messages.
Выключенный Copilot отвечает `provider copilot is disabled` с `reason: provider_disabled`, а не
непрозрачным `500`.

### Остальные провайдеры

| Эндпоинт | `type` | Обработчик | Конвертация |
|---|---|---|---|
| `/v1/messages` | `messages` | `makePassthroughHandler` | — |
| `/v1/messages` | `claude-cli` | `makeClaudeCLIHandler` | — |
| `/v1/messages` | `responses` + `convert_to_messages` | `makeMessagesToResponsesHandler` | Anthropic → Responses |
| `/v1/messages` | `chat` + `convert_to_messages` | `makeMessagesToChatHandler` | Anthropic → Chat |
| `/v1/responses` | `responses` | `makePassthroughHandler` | — |
| `/v1/responses` | `messages` + `convert_to_responses` | `makeResponsesToAnthropicHandler` | Responses → Anthropic |
| `/v1/responses` | `claude-cli` + `convert_to_responses` | `makeResponsesToAnthropicHandler` (транспорт — через CLI) | Responses → Anthropic |
| `/v1/chat/completions` | `responses`, `chat` | `makePassthroughHandler` | — |

Любая другая комбинация — `400` с `reason: endpoint_not_supported`. Обработчик, выбранный в
`Route`, вызывается из `ServeHTTP`, который перед этим добавляет в ответ `X-Provider` и
`X-Upstream-Model`.

Пасс-хендлер работает потоково: стриминговый ответ проксируется построчно с `Flush`, а не
буферизуется (провайдер вправе держать соединение открытым после терминального события, поэтому
выход — по `[DONE]` или по завершению события `message_stop`). Нестриминговые ответы
повторяются до трёх раз при `5xx` с экспоненциальной паузой.

## Пакеты

```
main.go                  точка входа: флаги, журнал, mux, инициализация OAuth, graceful shutdown
middleware.go            request ID + Record, CORS, latency-лог
ratelimit.go             лимитер на IP (100/сек), /health исключён
debug_middleware.go      отладочный лог запросов/ответов
admin.go                 /admin/stats, /admin/usage, -stats, -backfill

providers/
├── config.go            ProviderConfig, LoadConfig, Validate, InitOAuth, FindProvider, AdapterKey
├── router.go            Route, ServeHTTP, пасс-хендлеры, конвертирующие хендлеры, setAuthHeader
├── model.go             ModelInfo, ModelCapabilities
├── models.go            ModelsCache (/v1/models), FetchModels, FetchCopilotModels, populateEndpoints
├── model_aliases.go     model_aliases Copilot + встроенный codex-auto-review
├── adapter_types.go     интерфейс Adapter (PatchRequest/PatchResponse) + реестр
├── context.go           logger в контексте запроса
├── telemetry.go         запись фактов роутинга в Record (provider, model_resolved, reason)
├── responses_native.go  обёртка Anthropic SSE → Responses SSE
├── inertmcp.go          инертный MCP-сервер (инвентарь инструментов для claude-cli)
├── claudecode.go        Anthropic-запрос → кадры stream-json
├── claudecode_proc.go   spawn CLI: argv, whitelisted env, process group, чтение stdout
├── claudecode_config.go вход CLI: каталог, EnsureClaudeCLI, запуск auth login
├── claudecode_handler.go жизненный цикл одного хода, SSE, коды ошибок
├── claudecode_admission.go loopback-релей: одна попытка upstream, захват ответа
├── claudecode_cachewire.go восстановление tool-result кадра и перенос cache-брейкпоинта
├── claudecode_bridge.go мост /v1/responses → обработчик /v1/messages
├── claudecode_response.go сборка Anthropic-ответа из событий CLI
├── terminal_*.go        терминальные примитивы по платформам
├── adapters/deepseek.go DeepSeekAdapter: MCP namespace → плоские tools, нормализация имён
└── openai/{oauth,config}.go  OpenAI device flow и хранение токенов

anthropic/               Anthropic-типы, обработчик Copilot, конвертеры
├── convert.go           Anthropic ↔ Chat Completions
├── responses_convert.go Anthropic → Responses (запрос), Responses → Anthropic (ответ, стрим)
├── responses_reverse_convert.go       Responses → Anthropic (запрос)
├── responses_reverse_convert_resp.go  Anthropic → Responses (ответ, стрим)
├── responses_stream.go  Anthropic SSE → Responses SSE
├── handler.go           обработчик Copilot /v1/messages
├── models.go            каталог Anthropic-моделей Copilot
└── testdata/            эталонные фикстуры конвертеров (golden-тесты)

requestlog/
├── logger.go            Init/Get, Emit сводки, структура каталогов
├── record.go            Record, Recorder, Mutate, ClientName, BodyHash
├── transport.go         WrapTransport: запись proxy→upstream / upstream→proxy
├── capture.go           тела user→proxy и proxy→user
├── detect.go            распознавание пробы (probe) и служебных путей
├── elide.go             структурная обрезка JSON по краям
├── limits.go            BodyLimit, суточный бюджет
├── index.go             индекс SQLite, Stats (group_by/by), ValidateGroup
├── usage.go             извлечение usage из хвоста SSE/JSON
├── backfill.go          индексация существующих логов
└── retention.go         ретеншн по возрасту и объёму

auth/                    GitHub Copilot: device flow, Copilot token, хранение
internal/upstream/       транспорт Copilot, ParseProxyURL, contextTier, каталог моделей
internal/models/         кеш моделей Copilot
internal/modelid/        срезание display-суффиксов
internal/sse/            SSE-примитивы
internal/types/          общие типы OpenAI/Responses
proxy/                   обработчик прокси Copilot API (/v1/embeddings, /usage, AmpCode)
gemini/                  Gemini-совместимые эндпоинты
amplocal/, ampsearch/    AmpCode: локальные треды и поиск
```

## Конвертеры форматов

Три направления, все на Go, без внешних процессов:

| Файлы | Направление запроса | Направление ответа | Где используется |
|---|---|---|---|
| `anthropic/convert.go` | Anthropic → Chat Completions | Chat → Anthropic | `/v1/messages` у `type: "chat"` |
| `anthropic/responses_convert.go`, `responses_stream.go`, `providers/responses_native.go` | Anthropic → Responses | Responses → Anthropic | `/v1/messages` у `type: "responses"`; GPT-модели Copilot на `/v1/messages` |
| `anthropic/responses_reverse_convert.go`, `responses_reverse_convert_resp.go` | Responses → Anthropic | Anthropic → Responses | `/v1/responses` у `type: "messages"` и `claude-cli`; Claude-модели Copilot на `/v1/responses` |

Направления зеркальны, а не дублируют друг друга: каждое обслуживает свой путь
клиент ↔ провайдер.

Golden-тесты сверяют результат с фикстурами в `anthropic/testdata/reference`. Намеренные
отклонения описаны рядом с фикстурами (`DIFFERENCES.md`) и закреплены отдельными тестами
(`responses_reverse_deviation_test.go`).

Ключевые правила конвертеров:

- `instructions` (Responses) переносятся в `system`;
- `parallel_tool_calls: false` → `disable_parallel_tool_use`;
- reasoning передаётся и в стриме, и в нестриминговом ответе;
- неполный `usage` и неизвестные события не обрывают поток;
- поток всегда завершается `response.completed` / `incomplete` / `failed`;
- неподдерживаемые инструменты отбрасываются с предупреждением, а не роняют запрос.

## Адаптеры

`Adapter` (`providers/adapter_types.go`) правит запрос/ответ до и после транспорта, не трогая
общий роутер. Регистрация — по ключу `"type[sub_type]"` с fallback на `"type"`, поэтому
`RegisterAdapter("messages[deepseek]", …)` действует только на провайдера с
`sub_type: "deepseek"`.

`DeepSeekAdapter`:

- раскрывает MCP namespace-инструменты (`type: "namespace"`) в плоские `type: "function"`;
- нормализует имена моделей, срезая display-суффиксы;
- вызывается в `makePassthroughHandler` и `makeResponsesToAnthropicHandler` через
  `GetAdapter(p.Type, p.SubType)`.

## `claude-cli`: транспорт

Полное описание — [`docs/claude-cli.md`](docs/claude-cli.md). Инварианты, которые проверяются
тестами:

1. **Один upstream на запрос.** Admission-релей принимает только первый `POST /v1/messages`;
   последующие отбиваются локально (`429`), пробы слота не занимают.
2. **Статус не подменяется.** `200` коммитится лениво, на первом содержательном событии; отказ
   upstream до контента уходит своим статусом, оборванный стрим — `502`.
3. **Учёт не теряется.** Record входящего запроса передаётся релею явно, иначе хоп из другого
   процесса не виден и токены были бы нулевыми.
4. **Контекст модели не теряется.** `system` верхнего уровня попадает в
   `--system-prompt-file`; `thinking.display` и `signature_delta` пробрасываются.
5. **Кэш не деградирует.** `cwd` всех дочерних процессов постоянен, tool-result кадр
   восстанавливается, message-брейкпоинт переносится на повторяемый контент.
6. **Креды не текут.** Окружение — белый список; `HOME` подменён; файл кредов CLI не читается.
7. **Инструменты не исполняются.** `--tools ''` + инертный MCP: `tools/list` отдаёт инвентарь,
   `tools/call` отвечает `Denied`.

## Журнал запросов

Слои:

```
requestIDMiddleware                 создаёт Record, ставит его в контекст
  ├─ requestlog.WrapTransport       пишет proxy→upstream и upstream→proxy
  ├─ requestlog.capture             пишет user→proxy и proxy→user (структурная обрезка)
  └─ handlers                       дописывают факты: provider, model_resolved, reason, stream
Writer.Close / Emit                 сериализует сводку в requests-YYYY-MM-DD.jsonl
                                    + строку в index.sqlite + строку в errors-*.jsonl при ошибке
```

Важные решения:

- **Сводка пишется всегда**, тела — только при включённом `AIPROXY_LOG_BODIES`. Именно сводка
  отвечает на вопрос «кто сколько потратил», и это не должно зависеть от режима отладки.
- **Обрезка структурная.** `ElideBody` сохраняет начало и конец JSON, середину заменяет
  маркером с числом вырезанных байт; массивы сохраняют первые и последние элементы, длинные
  строки — свои концы. Строка остаётся валидным JSON.
- **Usage читается из хвоста.** Для SSE — `message_delta`, финальный чанк `usage`,
  `response.completed`; для JSON — поле `usage`. Чтение «первых N КБ» давало нулевые токены на
  стримах.
- **Пробы фильтруются.** `detect.go` распознаёт служебные GET-запросы (`/v1/models`, `/props`,
  `/api/tags`, `/version`, `/admin/*`) и не пишет их, если не включён `AIPROXY_LOG_PROBES`.
- **Ретеншн двухуровневый.** Возраст (`AIPROXY_LOG_RETENTION_DAYS`: сжатие в `.zst`, затем
  удаление) и объём (`AIPROXY_LOG_MAX_SIZE`: удаление самых старых суток, пока дерево не
  впишется). Текущие сутки не удаляются.
- **Индекс — SQLite** (`modernc.org/sqlite`, без cgo). Группировки — белый список
  (`ValidateGroup`), значения из запроса в SQL не подставляются.

## Инварианты

1. **Прокси нельзя обойти.** Заданный `proxy_host` валидируется при старте; ошибка разбора —
   отказ запуска, а не прямое соединение. Тесты (`proxy_no_fallback_test.go`) проверяют, что при
   мёртвом прокси upstream не получает ни одного запроса.
2. **Ключи не подменяются местами.** Входящий `Authorization`/`x-api-key` клиента вычищается и
   заменяется кредами провайдера.
3. **User-Agent клиента сохраняется.** Прокси не перезаписывает `User-Agent` (`claude-cli/…`
   важен для Anthropic); нейтральное значение ставится только при полном отсутствии.
4. **Ответственность за лимиты — у провайдера.** Тело запроса не усекается: 64 МиБ — это
   защита от runaway, а не нормализация.
5. **Таймаута на всю генерацию нет.** Ограничение — `ResponseHeaderTimeout` на заголовки и
   контекст клиента; долгая легитимная генерация не обрывается.
6. **Конфигурация только из `providers.json`.** Env-переменных для отдельных провайдеров нет;
   отсутствие файла — отказ запуска.
7. **Отказ объясним.** Каждый отказ без вызова upstream получает `reason` в журнале:
   `unknown_model`, `provider_disabled`, `endpoint_not_supported`, `unauthenticated`.

## Тесты

Каталоги тестов повторяют структуру пакетов; значимые группы:

| Файлы | Что фиксируют |
|---|---|
| `providers/config_test.go` | валидация конфига, взаимоисключения, дубли префиксов |
| `providers/claudecode_*_test.go` | кадры, replay, admission, cachewire на реальных телах CLI, workdir, system-поле, учёт токенов |
| `providers/passthrough_*_test.go`, `large_body_test.go`, `proxy_no_fallback_test.go` | стриминг, gzip, размеры тела, поведение прокси |
| `providers/model_aliases_test.go`, `native_anthropic_test.go`, `copilot_responses_native_test.go` | алиасы, роутинг Copilot |
| `anthropic/*_test.go` | конвертеры: unit, golden по фикстурам `testdata/reference`, отклонения, стрим |
| `requestlog/*_test.go` | обрезка тел, лимиты, индекс, статистика |
| `internal/*/*_test.go` | нормализация model id, транспорт, contextTier |

Запуск:

```bash
go build ./... && go vet ./... && go test ./...
```

## Как расширять

**Новый OpenAI/Anthropic-совместимый провайдер** — блок в `providers.json`: `type` по формату
API (`messages` / `responses` / `chat`), `base_url`, `api_key`, `model_prefix`. Код менять не
нужно.

**Провайдер с особенностями формата** — реализовать `Adapter` и зарегистрировать его по ключу
`"type[sub_type]"` в `main.go`.

**Новый тип транспорта** — новая ветка в `Validate()` и `Route()`, обработчик в `providers/`.
Ориентир — `claude-cli`: конфигурация, обработчик и вспомогательные файлы разделены по
назначению.

**Эндпоинт только для одного типа** — добавлять в `populateEndpoints` (`providers/models.go`),
чтобы каталог `/v1/models` не обещал того, чего роутер не обслуживает.
