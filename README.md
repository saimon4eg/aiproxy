# aiproxy

Мульти-провайдерный AI API прокси. Маршрутизирует запросы к GitHub Copilot, Anthropic, OpenAI, DeepSeek, Z.ai и любым OpenAI/Anthropic-совместимым API — всё через единый конфиг `providers.json`.

**Форк [copilot2api](https://github.com/whtsky/copilot2api) v0.4.0.** За документацией к базовому проекту — в оригинальный репозиторий.

## Эндпоинты

| Эндпоинт | Описание |
|----------|-------------|
| `/v1/chat/completions` | OpenAI Chat Completions (стриминг и обычный режим) |
| `/v1/responses` | OpenAI Responses API (нативный + конвертация в `/v1/messages` через linguafranca для anthropic-провайдеров с `convert_to_openai: true`) |
| `/v1/messages` | Anthropic Messages API (нативный + конвертация через linguafranca) |
| `/v1/models` | Агрегированный список моделей всех включённых провайдеров |
| `/v1/embeddings` | OpenAI Embeddings |
| `/v1beta/models` | Gemini-совместимый список моделей |
| `/health` | Статус провайдеров, доступность linguafranca |

## Быстрый старт

```bash
docker compose build
docker compose up -d
```

Прокси доступен на `http://localhost:8081`.

**Аутентификация OAuth-провайдеров происходит при старте сервера, последовательно.**

| Провайдер | Метод | Как работает |
|-----------|-------|--------------|
| **GitHub Copilot** (`copilot`) | Device code | Выводит ссылку и код. Открываете ссылку в браузере, вводите код на сайте. Сервер сам ждёт подтверждения — вводить в консоль ничего не нужно. |
| **OpenAI** (`openai`) | Device code | Аналогично: ссылка + код, вводите на сайте. Сервер сам опрашивает. |
| **Anthropic** (`anthropic`) | PKCE | Выводит ссылку. Открываете в браузере, авторизуетесь, копируете код со страницы и вставляете обратно в консоль. Для этого первый запуск должен быть через `docker compose run aiproxy`. |

Если включён только Copilot и/или OpenAI — `docker compose up` или `docker compose up -d`.
Если включён Anthropic OAuth — первый запуск через `docker compose run aiproxy`, после сохранения токенов — `docker compose up -d`.

## Конфигурация

Вся настройка провайдеров — в `providers.json`. Никаких переменных окружения для отдельных провайдеров: включение/выключение, ключи и настройки роутинга — в одном файле.

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
      "proxy_host": "localhost:2080"         // опциональный прокси
    },
    {
      "provider_id": "deepseek",
      "name": "DeepSeek",
      "type": "anthropic",
      "auth": "api_key",
      "enabled": true,
      "base_url": "https://api.deepseek.com/anthropic",
      "api_key": "sk-your-key",
      "model_prefix": "deepseek",
      "convert_to_openai": true,   // Responses ↔ Messages через linguafranca
      "proxy_host": "localhost:2080"
    }
  ]
}
```

### Поля провайдера

| Поле | Описание |
|-------|-------------|
| `provider_id` | Уникальный идентификатор. Для type-specific фич должен совпадать с именем адаптера |
| `type` | `copilot`, `anthropic` или `openai` |
| `auth` | `oauth` (device flow) или `api_key` |
| `enabled` | `true` — активировать при старте |
| `base_url` | URL upstream API. Обязателен для `anthropic`/`openai`. Для OAuth-провайдеров по умолчанию: `https://api.anthropic.com` и `https://api.openai.com` |
| `api_key` | API-ключ для `api_key`. Взаимоисключается с `auth: "oauth"` |
| `model_prefix` | Префикс для ID моделей, направляемых этому провайдеру (например `deepseek` → соответствует `deepseek-chat`) |
| `convert_to_openai` | Приём `/v1/responses` на anthropic-провайдере: Responses → Messages через linguafranca |
| `convert_to_anthropic` | Приём `/v1/messages` на openai-провайдере (заглушка, 501) |
| `proxy_host` | Прокси для всего трафика провайдера (например `localhost:2080`, `socks5://host:1080`). Пусто — напрямую. Задан, но недоступен — провайдер не работает (без fallback на прямое соединение) |
| `models` | Статический список моделей. Минимум — `id` (+ `capabilities` при необходимости); `object`, `vendor`, `supported_endpoints` подставляются автоматически. Если отсутствует — запрашивается с `base_url` |

### OAuth-провайдеры

При `auth: "oauth"` провайдер аутентифицируется через device code flow при первом запуске:

```
=== OpenAI Device Authorization ===
1. Open: https://auth.openai.com/codex/device
2. Enter code: XXXX-XXXX
```

Токены сохраняются в `~/.config/copilot2api/{provider}-credentials.json` и автоматически обновляются.

## Роутинг моделей

Модели маршрутизируются по префиксу:

```
copilot-gpt-5.4        → GitHub Copilot (префикс "copilot-" отрезается)
deepseek-v4-pro        → DeepSeek API (Anthropic-совместимый, через linguafranca)
anthropic-claude-opus  → Нативный Anthropic API (OAuth)
openai-gpt-5.2         → OpenAI API (OAuth)
zai-glm-5.2            → Z.ai API (Anthropic-совместимый)
```

`/v1/models` возвращает все модели всех включённых провайдеров, добавляя `{prefix}-` к каждому ID.

## Переменные окружения

| Переменная | Описание | По умолчанию |
|----------|-------------|---------|
| `COPILOT2API_HOST` | Хост сервера | `127.0.0.1` |
| `COPILOT2API_PORT` | Порт сервера | `7777` |
| `COPILOT2API_TOKEN_DIR` | Директория хранения токенов | `~/.config/copilot2api` |
| `COPILOT2API_DEBUG` | Отладочное логирование | `false` |

Никаких переменных окружения для настройки провайдеров. Всё в `providers.json`.

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
