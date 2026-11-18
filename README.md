# aiproxy

Мульти-провайдерный AI API прокси. Принимает запросы в форматах OpenAI Chat Completions,
OpenAI Responses и Anthropic Messages и направляет их к нужному провайдеру по префиксу ID
модели: GitHub Copilot, Anthropic, OpenAI, DeepSeek, Z.ai, Claude по подписке или любой
OpenAI/Anthropic-совместимый API.

Вся настройка провайдеров — в одном файле `providers.json`.

## Отношение к copilot2api

aiproxy — форк [copilot2api](https://github.com/whtsky/copilot2api) (база — v0.4.0, отдельные
изменения перенесены из более поздних upstream-релизов, см. `CHANGELOG.md`).

Что это значит на практике:

- **Copilot-часть идентична upstream.** OAuth GitHub → Copilot, проксирование `/v1/messages`,
  `/v1/chat/completions`, `/v1/responses`, `/v1/embeddings`, `/usage`, эндпоинты AmpCode
  (`/amp/*`, `/api/*`), Gemini-совместимый `/v1beta/models`, пакеты `auth/`, `proxy/`,
  `internal/*`, `gemini/`, `amplocal/`, `ampsearch/` — это код copilot2api. Поведение и
  тонкости этих эндпоинтов ищите в документации upstream-репозитория.
- **Наша часть — сверху.** Мульти-провайдерный роутер (`providers/`), конвертеры форматов
  (`anthropic/`), журнал запросов и статистика (`requestlog/`, `/admin/*`), провайдер
  `claude-cli` (Claude по подписке через официальный Claude Code CLI) и поля конфигурации,
  описанные ниже.
- Наша документация покрывает **использование aiproxy и нашу часть кода**. Для Copilot-специфики
  и внутренних деталей upstream обращайтесь к
  [github.com/whtsky/copilot2api](https://github.com/whtsky/copilot2api).

Техническое описание архитектуры — в [`ARCHITECTURE.md`](ARCHITECTURE.md).

## Возможности

| Возможность | Описание |
|---|---|
| Мульти-провайдерность | Любое число провайдеров в `providers.json`; роутинг по префиксу модели |
| Пять типов транспорта | `copilot`, `messages`, `responses`, `chat`, `claude-cli` |
| Конвертация форматов | Anthropic Messages ↔ OpenAI Responses ↔ OpenAI Chat Completions на Go |
| Агрегированный каталог | `/v1/models` собирает модели всех включённых провайдеров и добавляет префиксы |
| Три способа авторизации | OAuth device flow (Copilot, OpenAI), нативный логин Claude Code, статический API-ключ |
| Прокси на провайдера | `proxy_host` с `http://`, `https://`, `socks5://`, `socks5h://` |
| Журнал запросов | Сводка на запрос, тела с обрезкой по краям, индекс SQLite, ретеншн по возрасту и объёму |
| Статистика | `GET /admin/stats`, `GET /admin/usage` и CLI-режим `aiproxy -stats` |
| Честные ошибки | Отказ провайдера сохраняет свой HTTP-статус; причина отказа видна в журнале |

## Быстрый старт

Нужен Docker с Compose.

```bash
cp providers.template.json providers.json   # затем впишите ключи/включите нужные провайдеры
docker compose build
docker compose up -d
```

Прокси слушает `http://127.0.0.1:8081`.

Первый запуск — это ещё и авторизация OAuth-провайдеров, она выполняется последовательно
при старте процесса:

| Провайдер | Как авторизуется |
|---|---|
| GitHub Copilot (`copilot`) | Device flow: в логе печатается ссылка и код, вы подтверждаете их в браузере. Сервер сам ждёт подтверждения. |
| OpenAI (`openai`) | Device flow: ссылка `https://auth.openai.com/codex/device` и код в логе. |
| Claude по подписке (`claude-cli`) | Нативный вход официального CLI: `docker compose run --rm aiproxy` запускает `claude auth login`, вы подтверждаете вход, креды остаются в томе `auth-data/claude`. |
| Anthropic / DeepSeek / Z.ai / прочие | `api_key` в `providers.json`, интерактивного входа нет. |

Если включён `claude-cli` и входа ещё нет, обычный `docker compose up -d` (без TTY) не
поднимется — он скажет, что нужно выполнить `docker compose run --rm aiproxy`. После
сохранения входа поднимайте `docker compose up -d` как обычно.

Подробности по каждому механизму — в [`docs/authorization.md`](docs/authorization.md).

## Конфигурация

`providers.json` обязателен: без него процесс не стартует (env-переменных для отдельных
провайдеров нет). Пример:

```jsonc
{
  "providers": [
    {
      "provider_id": "copilot",
      "name": "GitHub Copilot",
      "type": "copilot",
      "auth": "oauth",
      "enabled": true,
      "model_prefix": "copilot",
      "integration_id": "copilot-developer-cli",
      "api_version": "2026-06-01",
      "proxy_host": "host.docker.internal:2080"
    },
    {
      "provider_id": "deepseek",
      "name": "DeepSeek",
      "type": "messages",
      "sub_type": "deepseek",
      "auth": "api_key",
      "enabled": true,
      "base_url": "https://api.deepseek.com/anthropic",
      "api_key": "sk-your-key",
      "model_prefix": "deepseek",
      "convert_to_responses": true
    },
    {
      "provider_id": "anthropic-claude",
      "name": "Anthropic Claude (Subscription)",
      "type": "claude-cli",
      "auth": "oauth",
      "enabled": true,
      "base_url": "https://api.anthropic.com",
      "model_prefix": "claude",
      "convert_to_responses": true,
      "models": [{ "id": "claude-sonnet-5-5", "name": "Claude Sonnet 5.5" }]
    }
  ]
}
```

### Поля провайдера

| Поле | Обязательное | Описание |
|---|---|---|
| `provider_id` | да | Уникальный идентификатор провайдера. Используется в `ByID`, `X-Provider` и в адаптерах (`type[sub_type]`). |
| `type` | да | Транспорт: `copilot`, `messages`, `responses`, `chat`, `claude-cli`. |
| `sub_type` | нет | Подтип для адаптера: ключ регистрации `"type[sub_type]"`, fallback на `"type"`. |
| `auth` | нет | `oauth` или `api_key`. Отсутствует — считается `api_key`. |
| `enabled` | да | `true` — провайдер активен. Выключенный провайдер запоминается: запрос на его префикс получает `reason: provider_disabled`, а не `unknown_model`. |
| `model_prefix` | да | Префикс ID моделей этого провайдера (`deepseek` → `deepseek-v4-pro`). Не может начинаться с `copilot-` и не может повторяться. |
| `base_url` | зависит | Upstream URL. Обязателен для `messages`, `responses`, `chat`. Для `claude-cli` по умолчанию `https://api.anthropic.com`. |
| `api_key` | зависит | Ключ статической авторизации. Взаимоисключается с `auth: "oauth"`. |
| `proxy_host` | нет | Прокси для всего трафика провайдера: инференс, OAuth, запрос каталога моделей. Без схемы — `http://`. Задан, но недоступен — провайдер не работает (fallback на прямое соединение отсутствует). |
| `models` | нет | Статический каталог моделей. Если не задан, `GET /v1/models` запрашивается у `base_url + "/models"`. Для `claude-cli` статический список нужен: у CLI нет своего эндпоинта каталога. |
| `convert_to_responses` | нет | `type: "messages"` или `type: "claude-cli"`: обслуживать `/v1/responses` через Go-конвертер Responses → Messages. |
| `convert_to_messages` | нет | `type: "responses"` или `type: "chat"`: обслуживать `/v1/messages` через Go-конвертер Anthropic → Responses или Anthropic → Chat. |
| `auth_header` | нет | Только `type: "messages"` при `api_key`: `x-api-key` (по умолчанию) или `bearer`. |
| `api_prefix` | нет | Префикс API у upstream вместо `/v1` (например `/v4` у Z.ai). Для `claude-cli` не поддерживается: CLI всегда зовёт `/v1/messages`. |
| `disable_keepalive` | нет | Отключить HTTP keep-alive у провайдера — обходной путь для upstream-багов с HTTP/2. |
| `integration_id` | нет | Только `copilot`: заголовок `Copilot-Integration-Id`. По умолчанию `copilot-developer-cli`. От идентичности зависит доступ к моделям, включая 1M-варианты. |
| `api_version` | нет | Только `copilot`: заголовок `X-Github-Api-Version`. По умолчанию `2026-06-01`. |
| `context_tier` | нет | Только `copilot`: поле `contextTier` для `/v1/responses` и `/v1/chat/completions`. Пусто — авто (`long_context` для моделей с окном ≥ 500k), `off` — не добавлять, иное значение — принудительно. Уже заданное клиентом поле не перезаписывается; нативный `/v1/messages` его не принимает. |
| `model_aliases` | нет | Только `copilot`: словарь «ID у клиента → модель Copilot без префикса». Работает на `/v1/responses` и `/v1/chat/completions`, совпадение точное. Не задан — действует встроенный `codex-auto-review → gpt-5.6-luna`; `{}` отключает алиасы; заданный словарь полностью заменяет встроенный. |
| `command` | нет | Только `claude-cli`: путь к бинарю Claude Code. По умолчанию `CLAUDE_CLI_PATH`, затем `claude` в `PATH`, затем типовые каталоги установки. |
| `read_idle_seconds` | нет | Только `claude-cli`: сколько секунд молчания допустимо до отказа от хода (по умолчанию 180). Это пауза, а не бюджет: пока идут дельты, генерация не обрывается. |
| `claude_config_dir` | нет | Только `claude-cli`: каталог, где CLI держит свой вход (`CLAUDE_CONFIG_DIR`). По умолчанию `<каталог токенов>/claude`. |

Валидация выполняется при старте и падает громко: неизвестный `type`, отсутствующий
`model_prefix`, дубли префиксов, `api_key` вместе с `auth: "oauth"`, нераспарсиваемый
`proxy_host`, `context_tier`/`model_aliases` у не-copilot провайдера — всё это отказ запуска,
а не тихая деградация.

### Что обслуживает каждый тип

| `type` | `/v1/messages` | `/v1/responses` | `/v1/chat/completions` |
|---|---|---|---|
| `copilot` | нативно (Claude) или конвертер (GPT) | нативно (GPT) или конвертер (Claude) | нативно |
| `messages` | нативно | `convert_to_responses: true` | — |
| `responses` | `convert_to_messages: true` | нативно | нативно |
| `chat` | `convert_to_messages: true` | — | нативно |
| `claude-cli` | нативно (через CLI) | `convert_to_responses: true` | — |

Неподдерживаемая комбинация — это `400` с `reason: endpoint_not_supported`, а не пустой ответ.
`/v1/models` показывает у каждой модели набор `supported_endpoints`, собранный из `type` и
флагов конвертации.

## Роутинг моделей

Модель берётся из поля `model` тела запроса:

```
copilot-gpt-5.4          → провайдер copilot (префикс copilot- срезается)
deepseek-v4-pro          → провайдер deepseek (префикс deepseek- срезается)
anthropic-api-claude-... → провайдер anthropic-api (префикс срезается, уходит x-api-key)
claude-sonnet-5-5        → провайдер claude-cli (ID не срезается: CLI ждёт нативное имя)
zai-msg-glm-5.2          → провайдер zai-coding-msg (base_url + /v1/messages)
```

Исключения и уточнения:

- **`claude-cli` сохраняет нативный ID.** Провайдер с `model_prefix: "claude"` отдаёт в
  каталоге и передаёт CLI ровно `claude-sonnet-5-5` — префикс не срезается и второй раз не
  добавляется.
- **Алиасы Copilot** (`model_aliases`) применяются до роутинга, для `/v1/responses` и
  `/v1/chat/completions`: `codex-auto-review` → `copilot-gpt-5.6-luna`.
- **Display-суффиксы Copilot** (`[1m]` и подобные) срезаются перед отправкой в Copilot.
- **`reason: provider_disabled`** — модель принадлежит выключенному провайдеру;
  `unknown_model` — префикс не принадлежит никому.

Заголовки ответа `X-Provider` и `X-Upstream-Model` сообщают, кто и какая модель реально
ответили (после алиасов, срезанных префиксов и суффиксов).

## Эндпоинты

| Эндпоинт | Назначение |
|---|---|
| `POST /v1/messages` | Anthropic Messages API |
| `POST /v1/responses` | OpenAI Responses API |
| `POST /v1/chat/completions` | OpenAI Chat Completions API |
| `GET /v1/models` | Агрегированный каталог моделей всех включённых провайдеров |
| `POST /v1/embeddings` | OpenAI Embeddings (Copilot) |
| `GET /v1beta/models`, `/v1beta/models/{model}` | Gemini-совместимый каталог (Copilot) |
| `GET /usage` | Квота GitHub Copilot (отдельный Copilot-эндпоинт upstream) |
| `GET /admin/stats` | Сводка из индекса журнала: `?since=7d&group_by=client&by=day&limit=30` |
| `GET /admin/usage` | Токены по ключам и моделям: `?since=7d` |
| `GET /health` | `{"status":"ok","providers":N}` |
| `/amp/v1/*`, `/api/provider/*`, `/api/*`, `/news.rss` | Совместимость с AmpCode и провайдер-специфичными путями клиентов (Copilot-часть upstream) |

Все эндпоинты инференса принимают стриминг (`"stream": true`) и отдают SSE.

## Авторизация

Три независимых механизма, по одному на провайдера:

| Механизм | Провайдеры | Где живут креды |
|---|---|---|
| OAuth device flow | `copilot`, `openai` | `$COPILOT2API_TOKEN_DIR/credentials.json` (GitHub + Copilot token), `$COPILOT2API_TOKEN_DIR/openai-credentials.json` |
| Нативный логин Claude Code | `claude-cli` | `$COPILOT2API_TOKEN_DIR/claude/.credentials.json` — пишет сам CLI, aiproxy токен не читает |
| Статический ключ | `messages`, `responses`, `chat` | `providers.json` → `api_key` |

Правила, которые проверяются кодом:

- `api_key` и `auth: "oauth"` взаимоисключающие — иначе отказ запуска.
- Провайдер с `auth`, отличным от `oauth`, обязан иметь `api_key` (кроме `copilot`).
- OAuth-токены обновляются автоматически; вход интерактивный и выполняется на старте, а не
  во время обслуживания запросов.
- Входящий `Authorization` клиента никогда не уезжает на upstream — прокси подставляет свои
  креды провайдера.

Полное описание потоков, файлов, ротации и типовых ошибок —
[`docs/authorization.md`](docs/authorization.md).

## Claude по подписке (`claude-cli`)

Провайдер `type: "claude-cli"` обслуживает запросы к Claude через **официальный Claude Code
CLI**, запущенный как модель-клиент на один запрос. Клиент говорит обычным Anthropic-форматом,
внутри aiproxy история превращается в кадры `stream-json`, а ответ транслируется обратно в SSE
с настоящими дельтами текста и reasoning.

Коротко о гарантиях:

- Один клиентский запрос = один процесс CLI = одно поколение на upstream (повторные попытки
  CLI отбиваются локально, поэтому подписка не списывается дважды).
- Инструменты не исполняются: CLI получает `--tools ''` и инертный MCP-инвентарь, любой
  `tools/call` отвечает `Denied`. Агентскую петлю ведёт клиент.
- Креды принадлежат CLI: aiproxy не читает, не хранит и не обновляет токен подписки.
- Окружение дочернего процесса — белый список: чужие `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`
  и `HOME` не пробрасываются.

Как это устроено внутри, что именно отключено у CLI, какие ограничения у формата и почему
такая схема корректна с точки зрения правил Anthropic —
[`docs/claude-cli.md`](docs/claude-cli.md).

## Прокси

`proxy_host` задаётся на провайдера и применяется ко всем его исходящим соединениям:
инференс, OAuth-обмен, запрос каталога моделей. Схемы: `http://`, `https://`, `socks5://`,
`socks5h://`; без схемы — `http://`.

- Значение валидируется при старте: нераспарсиваемое — отказ запуска.
- Прокси задан, но недоступен — запросы падают. Прямого соединения в обход прокси не бывает.
- Для `claude-cli` прокси прокидывается дочернему процессу через `HTTP(S)_PROXY`, а loopback
  всегда в `NO_PROXY`: внутренний релей слышит только `127.0.0.1`.
- SOCKS у CLI не поддерживается: при `socks5://` CLI запускается без прокси, в лог пишется
  предупреждение.

## Журнал запросов и статистика

На каждый запрос пишется одна строка-сводка в `logs/requests-YYYY-MM-DD.jsonl` и строка в
индекс `logs/index.sqlite`. Тела запросов и ответов пишутся отдельными файлами и только когда
включены (`AIPROXY_LOG_BODIES`, по умолчанию включаются вместе с `--debug`) — именно они дают
основной объём.

Поля сводки: `request_id, ts, method, path, model, model_resolved, provider, upstream_url,
status, error_code, error, upstream_called, reason, latency_ms, ttfb_ms, stream,
input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, client, profile,
session_id, task, origin, attempt, retry_of, body_hash, ratelimit, bytes_out`.

Что это даёт:

- **Токены есть и для стрима.** Usage читается из хвоста SSE (`message_delta`, финальный чанк
  `usage`, `response.completed`), а не из первых килобайт ответа.
- **Кто вызвал.** `client` — из `X-Hermes-Client` или `User-Agent`; `profile`, `session_id`,
  `task`, `origin` — из заголовков `X-Hermes-*`, если клиент их присылает.
- **Почему отказ.** `upstream_called: false` + `reason`: `unknown_model`, `provider_disabled`,
  `endpoint_not_supported`, `unauthenticated`, `upstream_error`.
- **Повторы.** `attempt` и `retry_of` считаются по хешу тела в скользящем окне — видно циклы
  ретраев, которых клиент не признаёт.
- **Квота провайдера.** Заголовки `*ratelimit*` и `retry-after` попадают в `ratelimit`.
- **Тела не забивают диск.** Строка тела ограничена `AIPROXY_LOG_BODY_LIMIT` (32 КБ) и хранит
  начало и конец, а середину заменяет маркер с точным числом вырезанных байт. JSON режется
  структурно: массивы сохраняют первые и последние элементы, длинные строки — свои концы,
  поэтому строка остаётся разбираемым JSON. Правило одно для всех провайдеров: `user→proxy`,
  `proxy→upstream`, `upstream→proxy`, `proxy→user`; у `claude-cli` добавляются хопы
  `cli→relay` и `relay→upstream`. Сверху суточный потолок `AIPROXY_LOG_BODY_BUDGET_MB`: когда
  он выбран, тела перестают писаться до смены суток, а сводки продолжают.
- **Пробы не засоряют.** GET на `/v1/models`, `/props`, `/api/tags`, `/version`, `/admin/*`
  и подобные по умолчанию не пишутся.

```bash
# сводка из индекса, без запуска сервера
aiproxy -stats -stats-since 7d -stats-group-by client
aiproxy -stats -stats-since 1d -stats-group-by model -stats-by hour

# проиндексировать существующие логи (разово)
aiproxy -backfill -backfill-since 90d

# то же по HTTP
curl 'localhost:8081/admin/stats?since=7d&group_by=task'
curl 'localhost:8081/admin/usage?since=7d'
```

`group_by`: `client`, `model`, `model_resolved`, `provider`, `task`, `profile`, `origin`,
`status`, `error_code`, `reason`, `path`. `by`: `hour`, `day`. `since`: `24h`, `7d` или дата
`2026-11-12`.

Ретеншн: суточные каталоги старше `AIPROXY_LOG_RETENTION_DAYS` сжимаются в `.zst`, старше двух
окон — удаляются; строки индекса старше двух окон тоже удаляются. Поверх работает потолок
объёма `AIPROXY_LOG_MAX_SIZE`: когда дерево перерастает его, удаляются самые старые суточные
каталоги и сводки, пока дерево не впишется. Текущие сутки не удаляются никогда. Уборка идёт в
процессе (`AIPROXY_LOG_RETENTION_INTERVAL`, по умолчанию раз в сутки, плюс проход на старте) —
cron в образе не нужен.

## Переменные окружения

| Переменная | Описание | По умолчанию |
|---|---|---|
| `COPILOT2API_HOST` | Адрес сервера | `127.0.0.1` |
| `COPILOT2API_PORT` | Порт сервера | `7777` |
| `COPILOT2API_TOKEN_DIR` | Каталог токенов и входа CLI | `~/.config/copilot2api` |
| `COPILOT2API_DEBUG` | Отладочное логирование | `false` |
| `AIPROXY_LOG_DIR` | Каталог журнала запросов | `logs` |
| `AIPROXY_LOG_BODIES` | Писать тела запросов/ответов | `true` при `--debug`, иначе `false` |
| `AIPROXY_LOG_BODY_LIMIT` | Потолок одной строки тела, байт | `32768` |
| `AIPROXY_LOG_BODY_BUDGET_MB` | Потолок тел на сутки, МБ | `512` |
| `AIPROXY_LOG_PROBES` | Писать пробы (`/v1/models`, `/props`, `/api/tags` …) | `false` |
| `AIPROXY_LOG_RETENTION_DAYS` | Окно хранения: старше — сжатие, вдвое старше — удаление | `14` |
| `AIPROXY_LOG_RETENTION_INTERVAL` | Период фоновой уборки (`24h`, `6h`, часы числом) | `24h` |
| `AIPROXY_LOG_MAX_SIZE` | Потолок всего каталога журнала (`20G`, `20480M`, байты) | `20G` |
| `AIPROXY_LOG_INDEX` | Индекс сводок `logs/index.sqlite` | `true` |
| `AMP_THREADS_DIR` | Каталог тредов AmpCode | `<каталог токенов>/threads` |
| `CLAUDE_CLI_PATH` | Путь к бинарю Claude Code для `claude-cli` | поиск по `PATH` и типовым каталогам |
| `CLAUDE_CLI_TIMEOUT` | Пауза молчания дочернего CLI, Go-duration (`180s`, `3m`); уступает `read_idle_seconds` провайдера | `180s` |

Флаги командной строки дублируют основные настройки (`-host`, `-port`, `-token-dir`, `-debug`,
`-log-dir`, `-log-bodies`, `-log-probes`, `-log-retention-days`, `-log-index`) и имеют
приоритет над переменными. Служебные режимы: `-version`, `-stats`, `-backfill`.

## Использование с клиентами

### Claude Code

```json
// ~/.claude/settings.json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://127.0.0.1:8081",
    "ANTHROPIC_API_KEY": "dummy"
  }
}
```

Модель указывается так, как она есть в `/v1/models`: для Copilot — `copilot-claude-sonnet-5-5`
(пойдёт нативным Messages-путём), для подписки — `claude-sonnet-5-5`.

### Codex

```toml
# ~/.codex/config.toml
model = "copilot-gpt-5.4"
model_provider = "aiproxy"

[model_providers.aiproxy]
name = "aiproxy"
base_url = "http://localhost:8081/v1"
wire_api = "responses"
api_key = "dummy"
```

Codex разговаривает Responses-форматом, поэтому модель Copilot на Claude или провайдер
`messages`/`claude-cli` требует `convert_to_responses: true` у провайдера.

### OpenAI SDK

```python
import openai

client = openai.OpenAI(api_key="dummy", base_url="http://localhost:8081/v1")
```

### Anthropic SDK

```python
import anthropic

client = anthropic.Anthropic(api_key="dummy", base_url="http://localhost:8081")
```

### Hermes

Провайдер Hermes настраивается как OpenAI-совместимый (`base_url = http://127.0.0.1:8081/v1`)
или как Anthropic-совместимый (`api_mode: anthropic_messages`,
`base_url = http://127.0.0.1:8081`). Имя модели — полное, с префиксом провайдера; для
`claude-cli` префикс `claude-` уже часть нативного ID.

## Поведение при ошибках

- Отказ upstream сохраняет свой статус и тело: `429`, `529`, `401` и прочие доходят до клиента
  без подмены на `200`.
- Ошибка конвертации — `400` с причиной, а не тихий пустой ответ.
- Все ответы инференса содержат `X-Request-ID`; журнал пишет тот же ID.
- Если стрим уже начался и статус отправить нельзя, клиент получает терминальное событие
  `error` с машинной причиной и полем `status`.

## Разработка

```bash
go build ./... && go vet ./... && go test ./...
docker compose build
```

Проверки в `scripts/` (нужны запущенный прокси и Python 3):

| Скрипт | Что делает |
|---|---|
| `codex_like_client.py` | Эмулятор Codex CLI: шлёт запросы формы Codex в `/v1/responses` (input-массив, `instructions`, reasoning, function- и MCP-namespace-инструменты, стрим, мультитёрн с `function_call_output`) и печатает матрицу результатов. `python3 scripts/codex_like_client.py --base http://127.0.0.1:8081 --model deepseek-v4-flash` |
| `verify_system_field.py` | Пост-деплойная проба `claude-cli`: доходит ли до модели поле `system` верхнего уровня и читается ли префикс из кэша на повторном запросе (`cache_read_input_tokens > 0`). `python3 scripts/verify_system_field.py --base-url http://localhost:8081 --model claude-sonnet-5-5` |

Структура пакетов, поток запроса, конвертеры и инварианты — в
[`ARCHITECTURE.md`](ARCHITECTURE.md). Инструкции для агентов и правила ведения changelog —
в [`AGENTS.md`](AGENTS.md).

## Лицензия

MIT — см. [`LICENSE`](LICENSE). Проект — форк [copilot2api](https://github.com/whtsky/copilot2api);
уведомление об авторских правах upstream сохраняется, права на изменения форка принадлежат их
авторам.
