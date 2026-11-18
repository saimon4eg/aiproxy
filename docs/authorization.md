# Авторизация

У aiproxy три независимых механизма авторизации. Выбор не глобальный, а на провайдера —
в поле `auth` соответствующего блока `providers.json`.

| Механизм | `auth` | Провайдеры | Кто владеет кредом | Где лежит |
|---|---|---|---|---|
| GitHub device flow → Copilot token | `oauth` | `type: "copilot"` | aiproxy | `$COPILOT2API_TOKEN_DIR/credentials.json` |
| OpenAI device flow (Codex CLI flow) | `oauth` | `type: "responses"` с `provider_id: "openai"` | aiproxy | `$COPILOT2API_TOKEN_DIR/openai-credentials.json` |
| Нативный вход Claude Code CLI | `oauth` | `type: "claude-cli"` | сам CLI | `$COPILOT2API_TOKEN_DIR/claude/.credentials.json` |
| Статический API-ключ | `api_key` | `messages`, `responses`, `chat` | вы | `providers.json` → `api_key` |

Общее правило: вход интерактивный и выполняется **при старте процесса**, а не в момент
обслуживания запроса. Входящий `Authorization` клиента никогда не пересылается на upstream —
прокси подставляет креды провайдера.

## 1. GitHub Copilot (`copilot`, `auth: "oauth"`)

Двухступенчатый поток:

```
1) GitHub device flow
   POST https://github.com/login/device/code        client_id=Iv1.b507a08c87ecfe98, scope=read:user
   → device_code, user_code, verification_uri, interval
   Пользователь вводит user_code на https://github.com/login/device
   POST https://github.com/login/oauth/access_token  grant_type=urn:ietf:params:oauth:grant-type:device_code
   → GitHub access token

2) Обмен на Copilot token
   GET https://api.github.com/copilot_internal/v2/token   Authorization: Bearer <github token>
   → короткоживущий Copilot token + base_url API
```

Что происходит на практике:

- при старте, если сохранённого GitHub-токена нет, в лог печатаются ссылка и код; сервер сам
  опрашивает GitHub (`authorization_pending` — ждём дальше, `slow_down` — увеличиваем интервал);
- Copilot token живёт ограниченное время и обновляется автоматически по сохранённому
  GitHub-токену, без участия человека;
- оба токена пишутся в `credentials.json` (`0600`) в каталоге токенов;
- если Copilot-провайдер выключен, аутентификация не запускается вообще.

Прокси провайдера применяется и к device flow, и к обмену токена — иначе вход не пройдёт в
сетях, где GitHub доступен только через прокси.

`integration_id` и `api_version` из `providers.json` задают идентичность клиента
(`Copilot-Integration-Id`, `X-Github-Api-Version`). От идентичности зависит доступ к моделям,
включая 1M-варианты контекста, поэтому менять их без причины не стоит.

## 2. OpenAI (`openai`, `auth: "oauth"`)

Поток device-auth, совместимый с Codex CLI (`client_id = codex-cli`), issuer
`https://auth.openai.com`:

```
POST /api/accounts/deviceauth/usercode     → device_auth_id, user_code, interval
Пользователь вводит код на https://auth.openai.com/codex/device
POST /api/accounts/deviceauth/token        (поллинг до 15 минут) → authorization_code + PKCE
POST /oauth/token                          grant_type=authorization_code → access/refresh/id token
```

- `access_token` — JWT; срок берётся из его `exp`, обновление выполняется заранее (за 5 минут
  до истечения) по `refresh_token`;
- токены хранятся в `openai-credentials.json` (`0600`);
- инициализация идёт в фоне при старте (`InitOAuth`), поэтому недоступность OpenAI на старте не
  блокирует остальные провайдеры — в лог пишется предупреждение;
- `InitOAuth` обслуживает OAuth только этим обработчиком: `provider_id` должен быть `openai`.

После входа провайдер работает как обычный Responses-провайдер: токен подставляется в
`Authorization: Bearer` при каждом запросе, плюс `convert_to_messages: true` даёт ему
`/v1/messages` через Go-конвертер.

## 3. Claude по подписке (`claude-cli`, `auth: "oauth"`)

Здесь aiproxy **не участвует в авторизации вовсе**. Креды принадлежат официальному Claude
Code CLI:

- каталог входа — `claude_config_dir` провайдера, иначе `CLAUDE_CONFIG_DIR`, иначе
  `<каталог токенов>/claude` (в Docker это том `auth-data/claude`);
- если в каталоге нет `.credentials.json`, при старте запускается `claude auth login` с
  подключённым терминалом;
- без терминала (обычный `docker compose up -d`) старт отказывает с инструкцией выполнить
  `docker compose run --rm aiproxy`;
- aiproxy не читает файл кредов, не хранит токен, не обновляет его и не передаёт по сети: с
  Anthropic общается сам CLI, обновление делает он же;
- дочернему процессу не передаются `HOME` и любые `ANTHROPIC_*` из окружения хоста, чтобы
  вход не мог «переехать» на другой аккаунт или на платный API-ключ.

Поэтому `auth: "oauth"` у `claude-cli` означает лишь «не нужен `api_key`»; никакого OAuth-кода
aiproxy для этого типа не исполняет, и `provider_id` тут ни при чём.

Подробности работы транспорта — [`claude-cli.md`](claude-cli.md).

## 4. Статический ключ (`auth: "api_key"`)

Самый простой случай: ключ лежит в `providers.json` и подставляется в заголовок по типу
транспорта.

| `type` | Заголовок |
|---|---|
| `messages` | `x-api-key: <ключ>` (по умолчанию) или `Authorization: Bearer <ключ>` при `auth_header: "bearer"`; дополнительно `anthropic-version: 2023-06-01` |
| `responses`, `chat` | `Authorization: Bearer <ключ>` |

Валидация при старте:

- `api_key` вместе с `auth: "oauth"` — отказ запуска (взаимоисключающие);
- `auth` не `oauth` и пустой `api_key` (кроме `copilot`) — отказ запуска;
- `api_key` вписывается в `providers.json`, который лежит в `.gitignore` — в репозиторий он не
  попадает; шаблон `providers.template.json` содержит пустые значения.

## Коды ответов, связанные с авторизацией

| Ситуация | Ответ |
|---|---|
| Входящий ключ прокси не совпал (если клиентская авторизация включена на уровне reverse-proxy) | `401` от этого слоя |
| Нет входа в Claude Code | `401` + инструкция `docker compose run --rm aiproxy` |
| OAuth-токен провайдера не получен, запрос всё же пришёл | `502` с причиной в теле и `reason: unauthenticated` в журнале |
| Anthropic вернул `authentication_failed` | `401` |
| Ключ истёк/отозван у API-провайдера | статус upstream без изменений (`401`/`403`) |

Проверка состояния входа — в журнале и логах процесса: `oauth initialized`, `copilot token
refreshed`, `claude cli login present`, `claude cli is not ready`.

## Ротация и перенос

- Copilot: удалить `credentials.json` и перезапустить — вход начнётся заново.
- OpenAI: удалить `openai-credentials.json` и перезапустить (или включить провайдер заново).
- Claude: удалить `<каталог токенов>/claude/.credentials.json` и выполнить
  `docker compose run --rm aiproxy`.
- Перенос на другую машину: достаточно перенести каталог токенов и `providers.json` (ключи
  `api_key` — в нём же). Файлы кредов создаются с правами `0600`, каталог входа CLI — `0700`.
