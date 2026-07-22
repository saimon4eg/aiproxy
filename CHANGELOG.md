# Список изменений

## [Unreleased]

### Исправления документации

- **ARCHITECTURE.md:** исправлена таблица роутинга — `convert_to_anthropic` для `openai` указан как «заглушка 501», но в коде это полноценный `makeMessagesToResponsesHandler` на Go-конвертерах. Добавлена секция «Конвертеры форматов» с объяснением что linguafranca и Go native — зеркала.
- **Критические изменения:** переименованы значения `type`: `"anthropic"` → `"messages"`, `"openai"` → `"responses"`. Флаги: `convert_to_openai` → `convert_to_responses`, `convert_to_anthropic` → `convert_to_messages`. Go-поля: `ConvertToOpenAI` → `ConvertToResponses`, `ConvertToAnthropic` → `ConvertToMessages`.
- **Адаптеры:** ключ регистрации `provider_id` → `"type[sub_type]"` с fallback на `"type"`. `RegisterAdapter("deepseek", ...)` → `RegisterAdapter("messages[deepseek]", ...)`. `GetAdapter(p.ProviderID)` → `GetAdapter(p.Type, p.SubType)`. Поле `SubType` (json: `"sub_type"`) в `ProviderConfig`.
- **Модели:** `supported_endpoints` удалены из `providers.json` — строятся автоматически из `type` + флагов конвертации в `populateEndpoints()`.

## v1.0.0 — 2026-07-06

Форк [copilot2api v0.4.0](https://github.com/whtsky/copilot2api). Полная переработка архитектуры: роутинг на основе провайдеров вместо жёстко зашитых путей.

### Новые возможности

- **Мульти-провайдерная архитектура.** Все провайдеры настраиваются в `providers.json`. Роутинг определяется по префиксу модели для каждого запроса — никаких зашитых путей под конкретных провайдеров.
- **Обобщённый роутер.** Единый `providers.Config` обрабатывает все маршруты `/v1/messages`, `/v1/chat/completions`, `/v1/responses`. `copilot-*` отрезает префикс и проксирует через copilot2api handler. Все остальные модели идут через своего провайдера.
- **Агрегация `/v1/models`.** Объединённый список моделей всех включённых провайдеров с полными capabilities (лимиты токенов, стриминг, tool calls, vision, reasoning effort).
- **Поддержка OAuth.** Anthropic (platform API key + подписка claude.ai), OpenAI (device code flow). Токены автоматически обновляются, хранятся для каждого провайдера в `~/.config/copilot2api/`.
- **linguafranca bridge.** Конвертация Responses API ↔ Anthropic Messages через Python `martian-linguafranca`. Доступна для любого провайдера с `type: "messages"` и `convert_to_responses: true`. Раскрытие MCP namespace для DeepSeek.
- **Адаптеры провайдеров.** Интерфейс `Adapter` для провайдер-специфичных правок запросов/ответов. `DeepSeekAdapter`: раскрытие MCP namespace, нормализация имён моделей, поддержка переподключения.
- **Прокси на каждый провайдер.** У каждого провайдера свой `proxy_host` (http/https/socks5). Нет `proxy_host` — прямое соединение.
- **Health-эндпоинт.** `/health` показывает статус провайдеров, доступность linguafranca.
- **Middleware.** Request ID, CORS, логирование задержек.

### Удалено

- Переменные окружения `DEEPSEEK_API_KEY`, `ANTHROPIC_ON`, `OPENAI_ON`. Все настройки провайдеров перенесены в `providers.json`.
- Пакет `deepseek/` — заменён обобщённым роутером и адаптером.
- Устаревший `loadFromEnv()`. `providers.json` теперь обязателен.

### Критические изменения

- Ключ DeepSeek API теперь в `providers.json` в поле `"api_key"`, а не в `DEEPSEEK_API_KEY`.
- Anthropic OAuth включается через `"enabled": true` + `"auth": "oauth"` в `providers.json`, а не `ANTHROPIC_ON=true`.
- Переменная `PROXY` удалена. Используйте `proxy_host` для каждого провайдера.
