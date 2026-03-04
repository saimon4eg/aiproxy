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
    ├─ префикс copilot-*   → отрезать префикс → copilot2api proxyHandler
    ├─ {префикс}-*         → FindProvider → отрезать префикс провайдера
    │   ├─ нативный формат   → проксирование к upstream
    │   └─ convert_to_openai → конвертация linguafranca → upstream
    └─ неизвестный          → ошибка 400
```

## Структура пакетов

```
main.go                  Точка входа, настройка mux, callback инициализации OAuth
middleware.go            Request ID, CORS, логирование задержек
providers.json           Конфигурация провайдеров

providers/
├── config.go            Загрузка конфига, валидация, InitOAuth, FindProvider
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
3. Регистрация адаптеров через `RegisterAdapter("deepseek", ...)`
4. Настройка прокси транспорта Copilot из `providers.json` (`cfg.ByID("copilot").ProxyHost`)
5. `providers.Config` регистрируется как HTTP-обработчик на эндпоинтах инференса

## Логика роутинга

`Route(r)` читает поле `"model"` из тела запроса, затем:

| Префикс модели | Действие |
|---|---|
| `copilot-*` | Отрезать префикс `copilot-`, отправить в обработчик copilot2api |
| `{провайдер}-*` | Отрезать префикс `{провайдер}-`, направить к upstream провайдера |

Для не-copilot провайдеров эндпоинт + тип провайдера определяет обработчик:

| Эндпоинт | Тип провайдера | Обработчик |
|---|---|---|
| `/v1/messages` | `anthropic` | Проксирование к `base_url` |
| `/v1/chat/completions` | `openai` | Проксирование к `base_url` |
| `/v1/responses` | `openai` | Проксирование к `base_url` |
| `/v1/messages` | `openai` + `convert_to_anthropic` | Заглушка (501, не реализовано) |
| `/v1/responses` | `anthropic` + `convert_to_openai` | Конвертация через linguafranca |

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

## linguafranca

`providers/linguafranca.go` управляет Python-подпроцессом `martian-linguafranca` для конвертации между форматами OpenAI Responses API и Anthropic Messages API. Доступен для любого провайдера с `type: "anthropic"` и `convert_to_openai: true`.

- Конвертация запроса: `linguafrancaConvertRequest(responsesJSON) → messagesJSON`
- Конвертация ответа: `linguafrancaConvertResponse(messagesJSON) → responsesJSON`
- Стриминг: `linguafrancaConvertStream(w, body)` прогоняет SSE-события через мост
- Таймаут 30с для обычных запросов, 5 минут для стриминга
- Graceful shutdown через `CommandContext`

## Адаптеры

Интерфейс `Adapter` предоставляет провайдер-специфичные правки запросов/ответов без изменения обобщённого роутера. Адаптеры регистрируются по `provider_id` и ищутся во время запроса.

`DeepSeekAdapter`:
- **Раскрытие MCP namespace** — преобразует инструменты типа `"type": "namespace"` в плоские `"type": "function"` (DeepSeek не понимает namespaced tools)
- **Нормализация имён моделей** — отрезает display-суффиксы (например `deepseek-v4-pro-xxx` → `deepseek-v4-pro`)
- Выполняется в `makePassthroughHandler` (перед upstream) и `makeResponsesToMessagesHandler` (перед конвертацией linguafranca)
