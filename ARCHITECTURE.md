# Архитектура

aiproxy — мульти-провайдерный AI API прокси. Принимает OpenAI/Anthropic-совместимые запросы и маршрутизирует их к нужному upstream API по префиксу ID модели.

## Поток запросов

```
Запрос клиента → middleware (request ID, CORS, задержка)
  → net/http ServeMux
    ├─ /v1/models          → providers.ModelsCache (агрегированный список)
    ├─ /v1/messages        → providers.Config.ServeHTTP (обобщённый роутер)
    ├─ /v1/chat/completions → providers.Config.ServeHTTP
    ├─ /v1/responses       → providers.Config.ServeHTTP
    ├─ /v1/embeddings      → copilot2api proxyHandler (проксирование)
    ├─ /v1beta/models      → gemini handler
    └─ /health             → встроенный обработчик

providers.Config.ServeHTTP
  → Route(r)
    ├─ copilot-*           → capability-driven routing (copilotModels)
    │   ├─ /v1/messages + Claude → copilotHandler (native passthrough)
    │   ├─ /v1/messages + GPT    → copilotAnthropicHandler (Anthropic→Responses Go)
    │   ├─ /v1/responses + GPT   → copilotHandler (native passthrough)
    │   └─ /v1/responses + Claude → copilotResponsesToMessages (linguafranca)
    ├─ {префикс}-*         → FindProvider → отрезать префикс провайдера
    │   ├─ нативный формат       → проксирование к upstream
    │   ├─ convert_to_responses     → linguafranca (Responses↔Anthropic)
    │   ├─ convert_to_messages (openai) → Go-конвертеры (Anthropic↔Responses)
    │   └─ convert_to_messages (chat)   → Go-конвертеры (Anthropic↔Chat)
    └─ неизвестный              → ошибка 400
```

## Структура пакетов

```
main.go                  Точка входа, настройка mux, callback инициализации OAuth
middleware.go            Request ID, CORS, логирование задержек
providers.json           Конфигурация провайдеров

providers/
├── config.go            Загрузка конфига, валидация, InitOAuth, FindProvider, AdapterKey
├── router.go            Обобщённый роутер: Route(), ServeHTTP, проксирование, конвертация
├── model.go             Типы ModelInfo, ModelCapabilities
├── models.go            ModelsCache, FetchModels, FetchCopilotModels
├── linguafranca.go      Мост к subprocess linguafranca (Responses ↔ Messages)
├── adapter_types.go     Интерфейс Adapter (NormalizeTools, PatchRequest, PatchResponse)
└── adapters/
    └── deepseek.go      Раскрытие MCP namespace, нормализация имён моделей

providers/openai/
├── oauth.go             Device code flow: запрос, опрос, обмен, обновление
└── config.go            Хранение токенов, LoadOrAuthenticate, авто-обновление

anthropic/               Обработчик Anthropic из copilot2api + OAuth (нативный PKCE/device)
├── oauth_config.go      Хранение токенов, LoadOrAuthenticate
└── oauth.go             PKCE flow, device flow, обмен токенов

auth/                    GitHub Copilot OAuth (device flow, хранение токенов)
internal/upstream/       Транспорт Copilot API, ParseProxyURL
internal/models/         Кеш моделей Copilot
proxy/                   Обработчик прокси Copilot API
amplocal/                Локальное управление тредами AmpCode
gemini/                  Gemini-совместимые эндпоинты
```

## Жизненный цикл провайдера

1. `main.go` загружает `providers.json` → `providers.Config`
2. `InitOAuth(callback)` обходит включённые провайдеры с `auth: "oauth"`, вызывает per-provider инициализацию:
   - `openai` → device code flow к `auth.openai.com`
   - `anthropic` → PKCE flow к `auth.anthropic.com`
3. Регистрация адаптеров через `RegisterAdapter("messages[deepseek]", ...)` — ключ `"type[sub_type]"` с fallback на `"type"`
4. Настройка прокси транспорта Copilot из `providers.json` (`cfg.ByID("copilot").ProxyHost`)
5. `providers.Config` регистрируется как HTTP-обработчик на эндпоинтах инференса

## Логика роутинга

`Route(r)` читает поле `"model"` из тела запроса, затем:

| Префикс модели | Действие |
|---|---|
| `copilot-*` | Отрезать префикс `copilot-`, отправить в обработчик copilot2api |
| `{провайдер}-*` | Отрезать префикс `{провайдер}-`, направить к upstream провайдера |

Для не-copilot провайдеров эндпоинт + тип провайдера определяет обработчик:

| Эндпоинт | Тип провайдера | Обработчик | Конвертер |
|---|---|---|---|
| `/v1/messages` | `messages` | Проксирование к `base_url` | — |
| `/v1/chat/completions` | `responses` / `chat` | Проксирование к `base_url` | — |
| `/v1/responses` | `responses` | Проксирование к `base_url` | — |
| `/v1/messages` | `responses` + `convert_to_messages` | Go-конвертеры (Anthropic↔Responses) | `anthropic.ConvertAnthropicToResponses` / `ConvertResponsesToAnthropic` / `TranslateResponsesStreamEvent` |
| `/v1/responses` | `messages` + `convert_to_responses` | linguafranca (Responses↔Anthropic) | `linguafrancaConvertRequest` / `linguafrancaConvertResponse` / `linguafrancaConvertStream` |
| `/v1/messages` | `chat` + `convert_to_messages` | Go-конвертеры (Anthropic↔Chat) | `anthropic.ConvertAnthropicToOpenAI` / `ConvertOpenAIToAnthropic` / `ConvertOpenAIChunkToAnthropicEvents` |

## Аутентификация

| Тип аутентификации | Как работает |
|---|---|
| `oauth` (copilot) | GitHub device flow → токен Copilot (обрабатывается `auth/` и `proxy/`) |
| `oauth` (openai) | OpenAI device flow → `auth.openai.com` → JWT токены (авто-обновление) |
| `oauth` (anthropic) | PKCE browser flow → API key/access token (авто-обновление) |
| `api_key` | Статический ключ из `providers.json` → отправляется как `x-api-key` или `Authorization: Bearer` |

## Прокси

У каждого провайдера есть опциональное поле `proxy_host`. Если задано — все HTTP-запросы этого провайдера (инференс, OAuth, получение моделей) идут через прокси. Поддерживаются схемы `http://`, `socks5://`, `socks5h://`. Если схема не указана — по умолчанию `http://`. Без `proxy_host` — прямое соединение.

**Никакого fallback на прямое соединение.** `proxy_host` валидируется при старте (`Validate()`): нераспарсиваемое значение — фатальная ошибка запуска. Если прокси задан, но недоступен в рантайме — запросы падают с ошибкой, провайдер не работает (Go не обходит заданный прокси напрямую).

Парсинг централизован в `internal/upstream.ParseProxyURL()`.

## Конвертеры форматов

В aiproxy два независимых конвертера для формата Anthropic Messages ↔ OpenAI Responses. Они **зеркальны**, не дубликаты — каждый обслуживает своё направление клиент↔провайдер:

| Конвертер | Направление запроса | Направление ответа | Направление стрима | Когда используется |
|---|---|---|---|---|
| **linguafranca** (Python/Rust) | Responses → Anthropic | Anthropic → Responses | Anthropic SSE → Responses SSE | Клиент говорит Responses, провайдер — Anthropic |
| **Go native** (`anthropic/responses_convert.go`) | Anthropic → Responses | Responses → Anthropic | Responses SSE → Anthropic SSE | Клиент говорит Anthropic, провайдер — Responses |

Плюс Go-конвертер для Chat Completions: `anthropic/convert.go` — Anthropic ↔ Chat Completions.

### linguafranca (Python/Rust bridge)

`providers/linguafranca.go` управляет Python-подпроцессом `martian-linguafranca` (`scripts/linguafranca_bridge.py`). Используется в двух хендлерах:

- `makeResponsesToAnthropicHandler` — для провайдеров с `type: "messages"` + `convert_to_responses: true`
- `makeCopilotResponsesToMessagesHandler` — для copilot Claude-моделей на `/v1/responses`

Режимы работы (`scripts/linguafranca_bridge.py`):
- `req` — `source=OPEN_RESPONSES`, `target=ANTHROPIC_MESSAGES` (запрос)
- `resp` — `source=ANTHROPIC_MESSAGES`, `target=OPEN_RESPONSES` (ответ)
- `stream` — `source=ANTHROPIC_MESSAGES`, `target=OPEN_RESPONSES` (стрим)

Таймаут 30с для обычных запросов, 5 минут для стриминга. Graceful shutdown через `CommandContext`.

### Go native конвертеры

`anthropic/convert.go` — Anthropic ↔ OpenAI Chat Completions:
- `ConvertAnthropicToOpenAI` — запрос Anthropic → Chat
- `ConvertOpenAIToAnthropic` — ответ Chat → Anthropic
- `ConvertOpenAIChunkToAnthropicEvents` — стрим Chat → Anthropic

`anthropic/responses_convert.go` — Anthropic ↔ OpenAI Responses:
- `ConvertAnthropicToResponses` — запрос Anthropic → Responses
- `ConvertResponsesToAnthropic` — ответ Responses → Anthropic
- `TranslateResponsesStreamEvent` — стрим Responses → Anthropic

Используются в:
- `makeMessagesToResponsesHandler` — для `type: "responses"` + `convert_to_messages`
- `makeMessagesToChatHandler` — для `type: "chat"` + `convert_to_messages`
- `copilotAnthropicHandler` — copilot GPT-модели на `/v1/messages`

## Адаптеры

Интерфейс `Adapter` предоставляет провайдер-специфичные правки запросов/ответов без изменения обобщённого роутера. Адаптеры регистрируются по ключу `"type[sub_type]"` (например `"messages[deepseek]"`) с fallback на `"type"`. Ключ провайдера вычисляется методом `AdapterKey()`: если задан `sub_type`, ключ = `"type[sub_type]"`, иначе просто `"type"`.

`DeepSeekAdapter` (ключ: `"messages[deepseek]"`):
- **Раскрытие MCP namespace** — преобразует инструменты типа `"type": "namespace"` в плоские `"type": "function"` (DeepSeek не понимает namespaced tools)
- **Нормализация имён моделей** — отрезает display-суффиксы (например `deepseek-v4-pro-xxx` → `deepseek-v4-pro`)
- Выполняется в `makePassthroughHandler` и `makeResponsesToAnthropicHandler` через `GetAdapter(p.Type, p.SubType)`
