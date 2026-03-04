# TODO: Поддержка previous_response_id

**Статус:** отложено
**Дата:** 2026-07-12

## Что это

`previous_response_id` — параметр OpenAI Responses API. Позволяет серверу хранить историю диалога: клиент шлёт только ID предыдущего ответа + новый `input`, сервер сам знает контекст. Экономия токенов на повторной передаче истории.

Документация: `POST /v1/responses`, поле `previous_response_id`. Требует `store: true` при создании первого ответа.

## Текущая ситуация

- **Copilot** — не поддерживает `store: true` (400), не хранит состояние
- **Codex** (exec и TUI) — не использует `previous_response_id` (подтверждено логами + strings бинарника)
- **DeepSeek / Anthropic API** — не используют `previous_response_id` в нативных `/v1/messages`

## Когда понадобится

При подключении **нативного OpenAI-провайдера** (не Copilot):

1. Первый запрос: `POST /v1/responses` + `store: true` → получаем `response.id`
2. Кэшируем `response.id` за `conv_id` (X-Conversation-ID)
3. Следующий запрос: вместо `input[]` с историей → `previous_response_id: "resp_123"` + только новый `input`
4. Upstream сам восстанавливает контекст → экономия токенов

**Роутинг:**

| Клиент | Формат | Действие |
|--------|--------|----------|
| Codex `/responses` | `previous_response_id` | passthrough для OpenAI native |
| Claude Code `/messages` | `messages[]` | если модель знает `/responses` → Messages→Responses с подстановкой `previous_response_id` |

## Что нужно реализовать

1. In-memory кэш: `conv_id → response_id` (TTL не нужен — сервер сам хранит)
2. Middleware для `/v1/responses`: при `previous_response_id` → passthrough, при первом запросе → `store: true` + сохранить `response.id`
3. Модификация `anthropicHandler`: подстановка `previous_response_id` вместо полной истории при повторе запроса
4. Заголовок `X-Conversation-ID` для связи запросов в одну сессию

## Не нужно

- Кэш для не-OpenAI провайдеров (они не поддерживают server-side state)
- Кэш с TTL (сервер хранит состояние, прокси только пересылает ID)
- Разворачивание истории из кэша прокси (это не нужно — клиент всегда шлёт историю сам, если нет `previous_response_id`)
