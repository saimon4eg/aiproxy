# Аудит плана универсальной конвертации

Аудитор: subagent. Дата: 2026-07-12.
Проверяемый план: `PLAN_UNIVERSAL_CONVERSION.md`.
Проект: `/home/saimon/projects/ai/aiproxy`.

## Резюме (Top-8)

| # | Severity | Finding |
|---|----------|---------|
| 1 | **[CRITICAL]** | Copilot API **не имеет** `/messages` endpoint. План предполагает native passthrough `/messages` → Copilot, но такого endpoint'а не существует. |
| 2 | **[CRITICAL]** | Step 4a добавляет `/v1/messages` в `supported_endpoints` Copilot Claude-моделей — это заставляет роутер (step 2) слать запросы в `copilotHandler`, который уходит в Copilot на несуществующий `/messages`. |
| 3 | **[CRITICAL]** | `makeCopilotResponsesToMessagesHandler` (step 3): для streaming использует `ResponseRecorder` (буферизует всё тело) + `linguafrancaConvertStream`, но `ResponseRecorder` несовместим со streaming — он дожидается конца ответа. |
| 4 | **[HIGH]** | Step 1 добавляет `/messages` в `proxy.Handler` как passthrough. Но `proxy.Handler` всегда шлёт в Copilot, а Copilot `/messages` не поддерживает. 100% ошибок на этом пути. |
| 5 | **[HIGH]** | `copilotModelEndpoints` (step 2) ссылается на несуществующий метод `c.copilotModelInfo(model)`. Кэш `providers.ModelsCache` хранит данные как `[]byte` — нет O(1) lookup по model ID. |
| 6 | **[HIGH]** | Два модельных кэша (`models.Cache` в internal/models и `providers.ModelsCache`) расходятся: аугментация endpoints в step 4a меняет ТОЛЬКО `providers.ModelsCache`, но `proxy.Handler.resolveTargetEndpoint` читает `models.Cache` (который не аугментирован). |
| 7 | **[HIGH]** | Openai-провайдер с `convert_to_anthropic=true` (step 4b): `/v1/messages` добавляется в model list, но handler возвращает 501 Not Implemented (`providers/router.go:70-72`). False advertising. |
| 8 | **[MEDIUM]** | Streaming-путь в `makeCopilotResponsesToMessagesHandler` (step 3): `linguafrancaConvertStream` пишет Responses SSE, но `proxy.Handler` для `/messages` вернёт OpenAI/Copilot SSE или ошибку — Anthropic SSE от Copilot не получить. |

---

## 1. Несоответствия протоколов (Protocol Inconsistencies)

### [CRITICAL] Copilot не имеет `/messages` endpoint — план предполагает обратное
- **План:** step 1 добавляет `/messages` в `proxy.Handler`, step 2 роутит Claude-модели на `copilotHandler` для `/messages` (native passthrough), step 3 вызывает `copilotHandler.ServeHTTP` с endpoint `/messages`
- **Реальность:** `proxy.Handler` → `upstream.Client.Do` → Copilot API (`https://api.githubcopilot.com`). Copilot API поддерживает `/chat/completions` и `/responses`, но **НЕ** `/messages`.
- **Файлы:** `proxy/handler.go:51-54` (endpoint switch), `internal/upstream/client.go:136` (baseURL + endpoint)
- **Последствия:** Любой `/v1/messages` запрос через `copilotHandler` (proxy.Handler) вернёт 404 или ошибку от Copilot.

### [CRITICAL] Step 4a: аугментация `/messages` для Copilot Claude-моделей
- **План:** `if hasResponses && !hasMessages { copilotModels[i].SupportedEndpoints = append(eps, "/v1/messages") }` — добавляет `/v1/messages` всем Copilot-моделям с `/v1/responses` (т.е. GPT и Claude).
- **Проблема:** Copilot Claude-модели НЕ поддерживают `/messages` нативно. Добавление этого endpoint'а заставляет роутер из step 2 отправлять запросы через `copilotHandler` (proxy.Handler), который шлёт их в Copilot. Результат: ошибка.
- **Файлы:** `providers/models.go:95-98` (FetchCopilotModels), `proxy/handler.go:51-54` (endpoint switch)

### [MEDIUM] Endpoint-строки в разных частях системы имеют разный формат
- **План:** `containsEndpoint(info.SupportedEndpoints, "/v1/messages")` и `containsEndpoint(info.SupportedEndpoints, "/responses")` — смешанные форматы.
- **В коде:** `proxy.Handler` использует endpoint без `/v1` (`"/chat/completions"`), `models.normalizeEndpoint` стрипит `/v1`. `providers/models.go` хранит `/v1/messages` (с `/v1`). `containsEndpoint` делает точное сравнение строк.
- **Файлы:** `proxy/handler.go:41` (`endpoint := strings.TrimPrefix(r.URL.Path, "/v1")`), `providers/models.go:249-256` (`containsEndpoint` — exact match), `internal/models/models.go:170-180` (`normalizeEndpoint`)
- **Риск:** Если `supported_endpoints` от Copilot приходят как `/v1/messages`, а где-то проверяется `/messages` — миматч.

---

## 2. Streaming vs Non-Streaming

### [CRITICAL] ResponseRecorder + streaming в `makeCopilotResponsesToMessagesHandler`
- **План (step 3):** `ResponseRecorder → c.copilotHandler.ServeHTTP(rec, req)` для получения ответа, затем `linguafrancaConvertResponse(rec.body)`.
- **Проблема:** `ResponseRecorder` (`providers/models.go:279-297`) буферизует ВЕСЬ ответ в память. Для streaming это означает: ждать пока весь стрим завершится, потом конвертировать. Клиент не получит ни одного SSE-события до полного завершения. Это ломает streaming семантику полностью.
- **Для streaming план говорит:** `linguafrancaConvertStream`. Но `linguafrancaConvertStream` (`providers/linguafranca.go:64-170`) ожидает на входе `io.ReadCloser` с Anthropic SSE событиями. Copilot не выдаёт Anthropic SSE.
- **Файлы:** `providers/models.go:279-297`, `providers/linguafranca.go:64-170`

### [MEDIUM] `isStreamingRequest` проверяется по телу, а не по URL/хедерам
- **План (step 3):** "Streaming — `linguafrancaConvertStream`" — без указания как определяется streaming.
- **В коде:** `proxy.Handler.handlePassthroughBody` вызывает `isStreamingRequest(bodyBytes)` которая читает `json:"stream"` из тела. В `providers/router.go:179-183` так же. Это корректно, но план не упоминает что для `/messages` после лингвафранка-конвертации поле `stream` может изменить имя/позицию.
- **Файлы:** `proxy/handler.go:114`, `providers/router.go:179-183`

---

## 3. Мульти-турн (Multi-Turn)

### [HIGH] linguafranca конвертация для copilot — потеря `previous_response_id`
- **План (step 3):** `linguafrancaConvertRequest(responsesBody)` → `messagesBody`. Не упоминает обработку `previous_response_id`.
- **Проблема:** Responses API использует `previous_response_id` для мульти-турн состояния. При конвертации в Messages этот идентификатор теряется. Для Copilot Claude-моделей это означает что каждый запрос — «с нуля», без контекста предыдущего ответа.
- **Файлы:** `providers/linguafranca.go:32-45` (просто отдаёт JSON в Python, не модифицирует), `proxy/convert.go:526-528` (логирует и игнорирует `previous_response_id`)

### [MEDIUM] Конвертация Assistant-сообщений: потеря `reasoning` блоков при Responses→Messages
- **План:** не упоминает.
- **В коде:** `anthropic/responses_convert.go:389-432` — `mapResponsesOutputToAnthropicContent` обрабатывает `reasoning` items (lines 394-401), но только для output (response). При конвертации input (истории) — `convertMessageToResponsesInputItems` (`responses_convert.go:89-98`) НЕ обрабатывает `reasoning` items в истории. Если клиент присылает Responses-историю с `reasoning` items, они будут потеряны при конвертации в Messages.
- **Риск:** низкий для первого сообщения, высокий для мульти-турн с reasoning.

---

## 4. Хедеры (Headers)

### [HIGH] `makeCopilotResponsesToMessagesHandler`: потеря клиентских хедеров
- **План (step 3):** Создаёт новый `*http.Request` и вызывает `c.copilotHandler.ServeHTTP(rec, req)`. Не копирует хедеры от оригинального запроса.
- **Проблема:** `proxy.Handler` использует `collectForwardHeaders(r)` (`proxy/handler.go:353-361`), который копирует только `Content-Type`, `Accept`, `Cache-Control`. Без них запрос может быть отклонён upstream'ом.
- **Фикс:** нужно `copyHeaders(newReq.Header, originalReq.Header)` перед отправкой.

### [MEDIUM] `anthropic-version` для copilot-моделей
- **План:** не упоминает.
- **В коде:** `providers/router.go:286-288` — `setAuthHeader` устанавливает `anthropic-version: 2023-06-01` только для `p.Type == "anthropic"`. Для Copilot этот хедер не ставится (Copilot не Anthropic API). Корректно.
- **Но:** если `makeCopilotResponsesToMessagesHandler` создаёт новый запрос и роутит его на `copilotHandler` (proxy.Handler), то `proxy.Handler` → `upstream.Client.Do` → `copilot.AddHeaders` — тоже не добавляет `anthropic-version`. Всё корректно для Copilot upstream.

### [LOW] `copyHeaders` пропускает `x-api-key` и `anthropic-version` — корректно
- **Файл:** `providers/router.go:312-325`
- Для non-copilot провайдеров это правильно (эти хедеры перезаписываются `setAuthHeader`). План не меняет эту логику.

---

## 5. Модельный кэш (Model Cache Divergence)

### [HIGH] Два кэша, одна аугментация
- **Кэш 1:** `models.Cache` (`internal/models/models.go`) — ключ `Info{ID, SupportedEndpoints}`, заполняется из upstream Copilot API через `upstream.Client.Do`. Используется `proxy.Handler.resolveTargetEndpoint` для smart routing.
- **Кэш 2:** `providers.ModelsCache` (`providers/models.go`) — ключ `ModelInfo` (богатая структура), заполняется из статических моделей + `FetchCopilotModels` (внутренний вызов proxy.Handler). Используется для `/v1/models` endpoint.
- **План (step 4a):** Аугментирует `SupportedEndpoints` только в `providers.ModelsCache` (в `FetchCopilotModels`).
- **Проблема:** `proxy.Handler.resolveTargetEndpoint` (`proxy/handler.go:166-205`) читает из `models.Cache` — который НЕ получает аугментацию из step 4a. Smart routing для prox`y.Handler будет видеть старые (неаугментированные) endpoints.
- **Файлы:** `proxy/handler.go:176` (`h.modelsCache.GetInfo`), `providers/models.go:91-98` (аугментация в FetchCopilotModels)
- **Практическое следствие:** `proxy.Handler` не будет знать что Claude-модели якобы поддерживают `/v1/responses` через linguafranca.

### [MEDIUM] `copilotModelInfo` — несуществующий метод
- **План (step 2):** `info := c.copilotModelInfo(model)` — этого метода нет в `providers.Config`.
- **В коде:** `providers.Config` (`providers/config.go:68-72`) имеет только `copilotHandler` и `copilotAnthropicHandler`. Никакого кэша моделей для O(1) lookup.
- **Нужно:** либо добавить поле `copilotModelsCache map[string]*copilotEndpoints` в Config и заполнять его при `FetchCopilotModels`, либо парсить `providers.ModelsCache` на каждый вызов.

---

## 6. Интеграционные разрывы (Integration Gaps)

### [CRITICAL] `makeCopilotResponsesToMessagesHandler` предполагает что Copilot имеет `/messages`
- **План (step 3):** `c.copilotHandler.ServeHTTP(rec, req)` с endpoint `/messages`.
- **Реальность:** `proxy.Handler` (copilotHandler) → `upstream.Client.Do` → `GET https://api.githubcopilot.com/messages` — этого endpoint'а нет в Copilot API.
- **Что должно происходить:** Для Claude-моделей на Copilot, `/v1/responses` должен проходить через ДВОЙНУЮ конвертацию: Responses → Messages (linguafranca), затем Messages → Chat/Responses (anthropic.Handler), затем → Copilot. Или использовать прямой Go-конвертер Responses → Chat Completions → Copilot.

### [HIGH] Step 1: `/messages` в `proxy.Handler` не имеет смысла без upstream, который его понимает
- **План (step 1):** Добавить `case "/messages": h.handlePassthrough(w, r, endpoint)` в `proxy.Handler`.
- **Проблема:** `handlePassthrough` отправляет запрос как есть в upstream Copilot. Copilot не понимает `/messages`. Этот case будет всегда возвращать ошибку.
- **Файлы:** `proxy/handler.go:46-58` (ServeHTTP switch), `proxy/handler.go:84-101` (handlePassthrough)

### [MEDIUM] `copilotAnthropicHandler` остаётся нужным, но план создаёт конфликт
- **План (step 2):** "NB: copilotAnthropicHandler — НЕ удаляется. Нужен для конвертации Messages→Responses (GPT на `/v1/messages`)."
- **Проблема:** После step 2, для Claude-моделей на `/v1/messages`: новый роутинг отправит в `copilotHandler` (потому что `supportsMessages=true` после аугментации). Но РАНЬШЕ они шли в `copilotAnthropicHandler` и работали. Теперь они пойдут в `copilotHandler` и сломаются.
- **Файлы:** `providers/router.go:38-41` (текущий хардкод, который работает), `main.go:127` (установка copilotAnthropicHandler)

### [MEDIUM] `providers/router.go` и `proxy/handler.go` — два разных роутера для одной модели
- **План:** добавляет сложную логику в `providers/router.go` для copilot-* моделей (capability-driven routing).
- **Проблема:** `proxy.Handler` УЖЕ имеет свой smart routing (`resolveTargetEndpoint`) для Chat↔Responses конвертации. При добавлении `/messages` в `proxy.Handler` (step 1), этот smart routing будет пытаться резолвить `/messages` в `/chat/completions` или `/responses`, что создаст конфликт с routing'ом в `providers/router.go`.
- **Файлы:** `proxy/handler.go:106-205` (smart routing), `providers/router.go:17-78` (Route)

---

## 7. Ответные форматы (Response Formats)

### [HIGH] Step 3: `linguafrancaConvertStream` пишет Responses SSE, но клиент на `/v1/responses` ожидает Responses SSE — ОК. Но upstream должен вернуть Anthropic SSE.
- **План:** `linguafrancaConvertStream(w, resp.Body)` — читает Anthropic SSE, пишет Responses SSE.
- **Проблема:** `proxy.Handler` (copilotHandler) для `/messages` (если бы работал) вернул бы ответ от Copilot. Copilot на `/messages` не отвечает. Если бы отвечал — формат был бы OpenAI/Copilot, не Anthropic. `linguafrancaConvertStream` ожидает `data: {...}` строки с Anthropic-форматом событий.
- **Файлы:** `providers/linguafranca.go:107-133` (ожидает Anthropic SSE `data: ` lines)

### [MEDIUM] Error format mismatch: `/v1/messages` → OpenAI error format в proxy.Handler
- **План (step 1):** `proxy.Handler` обрабатывает `/messages` как passthrough.
- **Проблема:** `proxy.Handler` использует `WriteOpenAIError` для ошибок (`proxy/handler.go:528-540`). Это OpenAI-формат `{"error": {"message": ..., "type": ...}}`. Но `/v1/messages` — Anthropic endpoint, клиенты ожидают `{"type":"error","error":{"type":"...","message":"..."}}`.
- **Файлы:** `proxy/handler.go:528-540`, `providers/router.go:381-395` (writeError — правильный формат по endpoint)

### [LOW] `makePassthroughHandler` копирует response headers без фильтрации
- **Файл:** `providers/router.go:150` — `copyHeaders(w.Header(), resp.Header)`
- Не опасно для текущего плана, но если upstream возвращает `Transfer-Encoding: chunked` или `Connection: keep-alive`, они попадут клиенту.

---

## 8. Edge Cases

### Empty body / модель не найдена

- **[MEDIUM] `copilotModelEndpoints` возвращает пустую структуру при `info == nil`:**
  - **План (step 2):** `if info == nil { return copilotEndpoints{} }`
  - **Результат:** Оба `supportsMessages` и `supportsResponses` = false. Роутер падает в `return c.copilotHandler, nil` (fallback). Если endpoint `/v1/messages` — proxy.Handler после step 1 попробует passthrough → ошибка от Copilot.
  - **Файлы:** `providers/router.go:47` (возврат ошибки "unknown model" для non-copilot, но copilot пропускает)

- **[LOW] Пустое тело:** `readBodyAndModel` (`providers/router.go:330-347`) вернёт ошибку "model field is required". План не меняет эту функцию.

### Гигантский body

- **[MEDIUM] План (step 3):** `io.ReadAll(io.LimitReader(r.Body, 10<<20))` — 10MB лимит.
- **Проблема:** После `linguafrancaConvertRequest` тело может измениться в размере (JSON с другими ключами). Если лингвафранка добавит системные поля — тело может превысить лимиты upstream'а. План не учитывает перепроверку размера после конвертации.
- **Файлы:** `providers/router.go:172` (LimitReader 10MB)

### Модель не найдена в кэше

- **[HIGH]** В step 2: `info := c.copilotModelInfo(model)` — если модель не найдена в кэше, возвращается пустой `copilotEndpoints{}`. Все проверки не проходят, роутинг падает в fallback `return c.copilotHandler, nil`. Для `/v1/messages` — сломанный passthrough. Для `/v1/responses` — ОК (Copilot поддерживает `/responses`).
- **Лучше:** fallback должен использовать `copilotAnthropicHandler` для `/v1/messages` (текущее поведение) и `copilotHandler` для остального.

### Capability отсутствует

- **[MEDIUM]** Step 2: если модель в кэше, но `SupportedEndpoints` пуст (баг в upstream `/v1/models`), `copilotModelEndpoints` возвращает `{false, false}`. Клиент получает fallback, который может сработать или нет в зависимости от endpoint'а.

### Double-read body

- **[MEDIUM]** `providers/router.go:18` — `readBodyAndModel` читает тело для извлечения model. Затем rewriteModel/stripPrefix пересоздают тело. Но downstream handler (`makePassthroughHandler`, `makeResponsesToMessagesHandler`) снова читает тело через `io.ReadAll`. Тело перечитывается 2 раза за запрос. План добавляет ещё один слой (в step 3), итого 3 чтения.

---

## Матрица тестов: проверка реалистичности

| Тест из плана | Реалистично? | Почему |
|---|---|---|
| `copilot-claude-sonnet-4.6` `/v1/messages` → native passthrough | **НЕТ** | Copilot не имеет `/messages`. Будет ошибка. |
| `copilot-claude-sonnet-4.6` `/v1/responses` → linguafranca → proxyHandler `/messages` | **НЕТ** | proxyHandler `/messages` → Copilot → ошибка. |
| `copilot-gpt-5.4` `/v1/messages` → anthropicHandler: Messages→Responses | **ОК** | Текущее поведение, план сохраняет. |
| `copilot-gpt-5.4` `/v1/responses` → native passthrough | **ОК** | Copilot поддерживает `/responses`. |
| `deepseek-deepseek-v4-flash` `/v1/messages` → native | **ОК** | Тип anthropic, passthrough на `/v1/messages`. |
| `deepseek-deepseek-v4-flash` `/v1/responses` → linguafranca | **ОК** | `convert_messages_to_responses=true`, существующий `makeResponsesToMessagesHandler`. |

---

## Заключение

План содержит **три критических** ошибки, делающих его неработоспособным:

1. **Copilot `/messages` не существует.** Step 1, step 3, и step 4a — все опираются на предположение что Copilot поддерживает Messages API. Это не так. Copilot API: `/chat/completions` и `/responses`.

2. **Streaming + ResponseRecorder несовместимы.** Step 3 использует `ResponseRecorder` для потоковых ответов — это буферизует весь ответ, ломая streaming.

3. **Аугментация ломает routing.** Step 4a добавляет `/v1/messages` Claude-моделям, заставляя step 2 направлять их в `copilotHandler` вместо работающего `copilotAnthropicHandler`.

**Чтобы план стал рабочим, минимально необходимо:**
- Step 1: убрать `/messages` из `proxy.Handler`.
- Step 2: не использовать `supportsMessages` для Copilot Claude (они всегда false).
- Step 3: `makeCopilotResponsesToMessagesHandler` должен конвертировать Responses → Chat Completions (через Go-конвертер `proxy.ConvertResponsesToChatRequest`), а не Responses → Messages.
- Step 4a: не добавлять `/v1/messages` Copilot Claude-моделям. Вместо этого для них `/v1/responses` должен работать через двойную конвертацию или прямой Go-конвертер.
