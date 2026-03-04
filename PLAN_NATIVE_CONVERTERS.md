# План: нативная Go-конвертация Responses↔Anthropic (замена linguafranca)

**Цель:** убрать Python-subprocess (`martian-linguafranca`) из DeepSeek/Z.ai/anthropic-api пути. Заменить тремя нативными Go-конвертерами — точными зеркалами существующих.

**Стек:** Go 1.26, `providers.json`, Docker Compose

**Файлы:** `anthropic/responses_reverse_convert.go` (новый), `anthropic/responses_reverse_stream.go` (новый), `anthropic/responses_reverse_convert_test.go` (новый), `providers/router.go`, `providers/linguafranca.go` (удаление), `Dockerfile`, `Dockerfile.ci`, `.env.example`, `main.go`, `scripts/linguafranca_bridge.py` (удаление)

**Референсы:** существующие зеркала — `anthropic/responses_convert.go` + `anthropic/stream.go` (TranslateResponsesStreamEvent); алгоритм reasoning — `moon-bridge/internal/protocol/anthropic/adapter.go` + extension `deepseek_v4` (не копируем код, используем как образец логики)

**Инвариант:** `providers` начинает импортировать `anthropic`. Цикла нет — проверено: `anthropic` тянет только `internal/*`, не `providers`.

---

## Что уже есть vs что нужно

| Направление | Есть (Copilot-поток) | Нужно (DeepSeek-поток) |
|---|---|---|
| **req** | `ConvertAnthropicToResponses` (Anthropic→Responses) | ❌ `ConvertResponsesToAnthropicRequest` (Responses→Anthropic) |
| **resp** | `ConvertResponsesToAnthropic` (Responses→Anthropic resp) | ❌ `ConvertAnthropicRespToResponsesResult` (Anthropic→Responses) |
| **stream** | `TranslateResponsesStreamEvent` (Responses→Anthropic SSE) | ❌ `TranslateAnthropicStreamEvent` (Anthropic→Responses SSE) |

---

## Задача 1: `ConvertResponsesToAnthropicRequest`

**Файл:** `anthropic/responses_reverse_convert.go` (новый)

Зеркало `ConvertAnthropicToResponses` (`responses_convert.go:36`).

```go
func ConvertResponsesToAnthropicRequest(req ResponsesRequest) (AnthropicMessagesRequest, error)
```

**Мапа полей:**

| ResponsesRequest | → | AnthropicMessagesRequest |
|---|---|---|
| `Model` | → | `Model` |
| `MaxOutputTokens *int` | → | `MaxTokens int` (nil → 4096) |
| `Instructions *string` | → | `System *AnthropicSystem` |
| `Input []ResponseInputItem` | → | `Messages []AnthropicMessage` |
| `Tools []ResponseTool` | → | `Tools []AnthropicTool` |
| `ToolChoice interface{}` | → | `ToolChoice *AnthropicToolChoice` |
| `Temperature` / `TopP` | → | `Temperature` / `TopP` |
| `Reasoning *ResponseReasoning{Effort}` | → | `Thinking *AnthropicThinking` |
| `Stream` | → | `Stream` |
| `Metadata` | → | `Metadata` |

**Хелперы (новые, зеркала существующих `convert*`):**
- `convertResponsesInputToMessages` — `ResponseInputItem` (message / function_call / function_call_output / reasoning) → `[]AnthropicMessage` (user / assistant с text / tool_use / tool_result / thinking-блоки)
- `convertResponsesToolsToAnthropic` — `[]ResponseTool` → `[]AnthropicTool`
- `convertResponsesToolChoiceToAnthropic` — `interface{}` → `*AnthropicToolChoice`
- `reasoningEffortToThinking` — обратный `resolveReasoningEffort`: Effort + Summary → `AnthropicThinking`

**⚠️ Фиделити reasoning (главный риск):** как Responses `reasoning.effort` мапится в Anthropic `thinking` — сверить с реальным выводом linguafranca (`req` mode) на примере DeepSeek-запроса + с Moon Bridge `deepseek_v4` расширением (`PrependThinkingToAssistant`, `encrypted_content` replay). Не угадывать — писать golden-тест (см. Задачу 6).

---

## Задача 2: `ConvertAnthropicRespToResponsesResult`

**Файл:** `anthropic/responses_reverse_convert.go` (новый)

Зеркало `ConvertResponsesToAnthropic` (`responses_convert.go:364`).

```go
func ConvertAnthropicRespToResponsesResult(resp AnthropicMessagesResponse) ResponsesResult
```

**Мапа полей:**

| AnthropicMessagesResponse | → | ResponsesResult |
|---|---|---|
| `ID` | → | `ID` |
| `Model` | → | `Model` |
| — | → | `Object: "response"` |
| `Content []AnthropicContentBlock` | → | `Output []ResponseOutputItem` |
| `StopReason` | → | `Status` + `IncompleteDetails` |
| `Usage` | → | `Usage *ResponsesUsage` |
| — | → | `OutputText` (собрать из text-блоков) |
| — | → | `CreatedAt: time.Now().Unix()` |

**Хелпер:** `mapAnthropicContentToResponsesOutput` — зеркало `mapResponsesOutputToAnthropicContent`:
- `text`-блок → `ResponseOutputItem{Type:"message", Role:"assistant", Content:[{Type:"output_text", Text:...}]}`
- `tool_use`-блок → `ResponseOutputItem{Type:"function_call", CallID, Name, Arguments}`
- `thinking`-блок → `ResponseOutputItem{Type:"reasoning", ID, Summary?, EncryptedContent?}`

---

## Задача 3: `TranslateAnthropicStreamEvent`

**Файл:** `anthropic/responses_reverse_stream.go` (новый)

Зеркало `TranslateResponsesStreamEvent` (`anthropic/stream.go:11`).

```go
type AnthropicToResponsesStreamState struct { ... }
func TranslateAnthropicStreamEvent(
    event AnthropicStreamEvent,
    state *AnthropicToResponsesStreamState,
) []ResponseStreamEvent
```

**Мапа SSE-событий:**

| Anthropic SSE | Responses SSE |
|---|---|
| `message_start` | `response.created` + `response.in_progress` |
| `content_block_start` (text/tool_use/thinking) | `response.output_item.added` |
| `content_block_delta` (text_delta) | `response.output_text.delta` |
| `content_block_delta` (input_json_delta) | `response.function_call_arguments.delta` |
| `content_block_delta` (thinking_delta) | reasoning-delta в output_item |
| `content_block_delta` (signature_delta) | `encrypted_content` в reasoning |
| `content_block_stop` | флаг завершения блока |
| `message_delta` (stop_reason/usage) | накопить в state |
| `message_stop` | `response.completed` (usage + stop_reason) |
| `error` | `error` |

**`state`** аккумулирует: ID ответа, модель, индекс текущего блока, накопленные аргументы tool-call, сигнатуру thinking, usage, stop_reason. Аналогичен `ResponsesStreamConvertState` в `proxy/stream.go`.

**MCP namespace:** встроить в конвертер (перенос из linguafranca.go:154-157) — для каждого `function_call`-output звать `types.ResolveMcpToolNamespace(Item.Name)` и проставлять `Item.Namespace`.

---

## Задача 4: Переписать `providers/router.go`

**Файл:** `providers/router.go`, функция `makeResponsesToMessagesHandler`

Добавить импорт `"github.com/whtsky/copilot2api/anthropic"` в `router.go`.

Три точки замены:

1. **Строка 182** (`linguafrancaConvertRequest(body)`):
```go
// Было:
messagesBody, err := linguafrancaConvertRequest(body)
// Стало:
var respReq anthropic.ResponsesRequest
if err := json.Unmarshal(body, &respReq); err != nil {
    writeError(w, r.URL.Path, http.StatusBadRequest, "invalid Responses JSON")
    return
}
anthropicReq, err := anthropic.ConvertResponsesToAnthropicRequest(respReq)
if err != nil {
    logger.Error("responses-to-anthropic conversion failed", "error", err, "provider", p.ProviderID, "model", reqMeta.Model)
    writeError(w, r.URL.Path, http.StatusBadGateway, "conversion failed")
    return
}
messagesBody, err := json.Marshal(anthropicReq)
```

2. **Строка 258** (`linguafrancaConvertResponse(respBody)`):
```go
// Было:
result, err := linguafrancaConvertResponse(respBody)
// Стало:
var anthResp anthropic.AnthropicMessagesResponse
if err := json.Unmarshal(respBody, &anthResp); err != nil {
    writeError(w, r.URL.Path, http.StatusBadGateway, "invalid Anthropic response")
    return
}
responsesResult := anthropic.ConvertAnthropicRespToResponsesResult(anthResp)
result, err := json.Marshal(responsesResult)
```

3. **Строка 229** (`linguafrancaConvertStream(w, resp.Body)`):
```go
// Было:
if err := linguafrancaConvertStream(w, resp.Body); err != nil { ... }
// Стало: нативный стрим-цикл —
// Сканировать Anthropic SSE из resp.Body → для каждой data: строки
//   json.Unmarshal в AnthropicStreamEvent
//   для каждого ResponseStreamEvent из TranslateAnthropicStreamEvent(event, &state)
//     писать "event: TYPE\ndata: JSON\n\n" + flush
// MCP namespace пост-процессинг — в самом конвертере (Задача 3)
```

---

## Задача 5: Удаление Python-инфраструктуры

**Только после верификации golden-тестов и smoke-проверки.**

| Действие | Файл |
|----------|------|
| Удалить целиком | `providers/linguafranca.go` |
| Удалить целиком | `scripts/linguafranca_bridge.py` |
| `Dockerfile`: убрать `python3 python3-pip`/`python3-venv`, `pip install`, `ENV PATH`, `LINGUAFRANCA_BRIDGE` | `Dockerfile` |
| `Dockerfile.ci`: убрать то же | `Dockerfile.ci` |
| `.env.example`: убрать `LINGUAFRANCA_BRIDGE` + `AMP_THREADS_DIR` (если ещё есть) | `.env.example` |
| `main.go`: убрать `/health` linguafranca-чек, `linguafrancaAvailable`, `sync/atomic` импорт (если больше не нужен) | `main.go` |

Попутно закрываются из FIX_PLAN.md:
- **M6** — `/health` форкает Python (Python больше нет → баг не существует)
- **L7** — Docker `--break-system-packages` (Python больше нет → не нужен ни pip, ни venv)
- **M15** — CI образ без linguafranca (Python больше нет → `FROM scratch` снова работает)

**Результирующий Docker-образ:** чистый Go-бинарь, без Python, без pip, без venv. Zero external dependencies кроме `ca-certificates`.

---

## Задача 6: Тесты

**Файл:** `anthropic/responses_reverse_convert_test.go` (новый)

Table-driven для каждого конвертера:
- `TestConvertResponsesToAnthropicRequest`: text-only, tool_use, function_call_output, reasoning/thinking, temp/top_p, max_tokens nil→дефолт, instructions→system
- `TestConvertAnthropicRespToResponsesResult`: text reply, tool_calls, thinking→reasoning, stop_reason→status, usage мап
- `TestTranslateAnthropicStreamEvent`: полный жизненный цикл message_start → content_block_start → delta → stop → message_stop

**Golden-тест (ключевой):**
1. Взять реальный DeepSeek-запрос в формате Responses
2. Прогнать через старый `linguafrancaConvertRequest` (ещё живой) → получить Anthropic JSON
3. Прогнать через новый `ConvertResponsesToAnthropicRequest` → получить Anthropic JSON
4. `assert json.Equal(oldResult, newResult)` с разумным допуском (`omitempty`, порядок полей)
5. То же для ответа (Anthropic→Responses) и стрима

Делать **до** удаления linguafranca. Если golden расходится — разбираться с фиделити reasoning/thinking/encrypted_content.

---

## Задача 7: Верификация

```bash
go build ./... && go vet ./... && go test ./... -count=1
docker build .  # без Python-слоя — должен собраться
docker compose build
```

Smoke-тест через Codex на провайдере deepseek:
```
curl http://localhost:8081/v1/responses -d '{"model":"deepseek-v4-pro","input":"hello","stream":false}'
curl http://localhost:8081/v1/responses -d '{"model":"deepseek-v4-pro","input":"hello","stream":true}'
```

---

## Порядок выполнения

1. Задачи 1–3 (конвертеры) + Задача 6 (тесты, golden против linguafranca)
2. Сверка golden — если расхождения, итерация фиксов
3. Задача 4 (переключить router.go) → `go build/vet/test`
4. Smoke: non-stream + stream через DeepSeek
5. Задача 5 (удаление Python) → `docker build`
6. `docker compose build` + повторный smoke

**Не удалять linguafranca, пока golden не сойдётся и smoke не пройден.**

---

## Риски и решения

| Риск | Решение |
|------|---------|
| **Фиделити reasoning/thinking** — linguafranca и наш конвертер могут по-разному мапить `encrypted_content`, `signature`, thinking-replay | Golden-тест против linguafranca + Moon Bridge `deepseek_v4` как референс |
| **Стрим-состояние** — накопление tool-call аргументов и thinking-сигнатуры может отличаться между конвертерами | Golden-тест стрима + сверка с `ResponsesStreamConvertState` логикой |
| **Типы не покрывают все поля** — `ResponsesRequest` имеет поля (`text`, `previous_response_id`, `store`), которых нет в Anthropic | Падать с ошибкой на неподдерживаемых полях (честный 400/501), не терять молча |
| **MCP namespace** — перенос из linguafranca.go в новый стрим-конвертер может потерять Nuance | Дословный перенос `ResolveMcpToolNamespace` + тест на function_call с namespace-префиксом |
