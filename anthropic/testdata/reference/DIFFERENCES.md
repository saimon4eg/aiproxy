# Намеренные отклонения нативного конвертера Responses → Anthropic от эталонных фикстур

Эталон — фикстуры в этом каталоге (`req/`, `resp/`, `stream/`).
Golden-тесты (`anthropic/responses_reverse_golden_test.go`) требуют точного совпадения, кроме
перечисленного ниже; каждое отклонение проверяется отдельным тестом в
`responses_reverse_deviation_test.go`.

## Запрос (Responses → Anthropic)

| Что | Эталон | Нативный | Почему |
|---|---|---|---|
| `instructions` | теряется (warning `dropped`) | → `system` (перед system-сообщениями из `input`) | Системный промпт Codex не должен пропадать |
| `parallel_tool_calls: false` | теряется | `tool_choice.disable_parallel_tool_use = true` | Anthropic поддерживает |
| `user` | теряется | → `metadata.user_id` | Anthropic поддерживает |
| `function_call.arguments` не объект (`[1,2]`) | пробрасывается массивом | `{}` + warning | `tool_use.input` обязан быть объектом |
| `input_file.file_data` с префиксом `data:...;base64,` | префикс остаётся в base64 | префикс снимается | Иначе Anthropic отвергает документ |
| `refusal` в сообщении ассистента | ошибка схемы | воспроизводится как текст | Не ломать мультитёрн |
| `reasoning.summary: "none"` | ошибка схемы | `thinking.display = omitted` | Допустимое значение API |
| неподдерживаемый инструмент (`local_shell`, `file_search`) | запрос падает целиком | инструмент отбрасывается, warning | Остальные инструменты работают |
| инструмент `function` во вложенной форме (`{"function": {...}}`) | ошибка | принимается | Часть клиентов шлёт Chat-форму |
| `tool_choice` во вложенной форме | теряется | учитывается | Тот же случай |
| `custom`-инструмент | пустой `tools: []` | поле `tools` не отправляется | Пустой массив отвергается API |
| пустой `input_text` | пустой text-блок | пропускается | Anthropic отвергает пустые text-блоки |
| `service_tier` | `"standard"` | не пробрасывается | Значения зависят от провайдера |
| неверный запрос (`input` нет, `max_output_tokens < 1`) | ошибка | ошибка, HTTP 400 | Клиент получает понятную причину, а не 502 |

## Ответ (Anthropic → Responses)

| Что | Эталон | Нативный | Почему |
|---|---|---|---|
| ID элементов (`rs_`, `msg_`, `fc_`) | случайные | детерминированные (SHA-256 от id ответа) | Воспроизводимые тесты и логи |
| `created_at` | `0` | реальное время | `0` выглядит как баг |
| `stop_reason: stop_sequence` | `incomplete` | `completed` | Это штатное завершение |
| thinking без signature | отбрасывается | сохраняется как reasoning-summary без `encrypted_content` | Не терять видимые рассуждения |

## Стрим (Anthropic SSE → Responses SSE)

| Что | Эталон | Нативный | Почему |
|---|---|---|---|
| Блоки `thinking` | не выводятся | `reasoning` item + `reasoning_summary_*` события, `signature_delta` → `encrypted_content` | Нужны для replay в следующем ходе |
| `message_delta` без `input_tokens` | падает (`SchemaValidationError`) | допускается | Провайдеры шлют урезанный usage |
| Неизвестные события | падает | пропускаются | Устойчивость |
| `ping` | не-стандартное событие `keepalive` | не пробрасывается | Нет такого события в Responses API |
| Пустые аргументы `tool_use` | `""` | `"{}"` | Валидный JSON |
| Статус `output_item` при `max_tokens` | `incomplete` | `completed` (сам `response` — `incomplete`) | Причина остановки приходит после закрытия блока |
| `usage` в `response.completed` без `message_delta` | отсутствует | берётся из `message_start` | Не терять токены |
| Обрыв потока без финального события | поток обрывается | `response.failed` | Клиент не должен зависать |
| `error` посреди потока | `response.failed` | `response.failed` + `usage` | Учёт расхода |
