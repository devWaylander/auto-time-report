# API Jira и WorklogPRO

Публичной документации на REST API WorklogPRO для Jira Server/Data Center нет: всё ниже снято с запросов браузера в DevTools и проверено на живой Jira. Домены и логины здесь - плейсхолдеры.

Реализация - [internal/jira/jira.go](../internal/jira/jira.go).

## Авторизация

Personal Access Tokens у админов выключены, работает **Basic auth** логином и паролем прямо в каждом запросе:

```
Authorization: Basic base64(login:password)
X-Atlassian-Token: no-check
```

`X-Atlassian-Token: no-check` отключает XSRF-проверку Jira для скриптов. Без него формы требуют `atl_token` из браузерной сессии.

Признаки успеха и провала - в заголовках ответа:

| Заголовок | Значение |
|---|---|
| `X-Seraph-LoginReason` | `OK` - вошли; `AUTHENTICATED_FAILED` - неверный пароль |
| `X-AUSERNAME` | под каким пользователем сервер нас узнал |
| `X-Authentication-Denied-Reason` | например `CAPTCHA_CHALLENGE` - после нескольких неудачных входов |

**Важно:** после нескольких неудачных логинов Jira включает капчу и вход по Basic auth перестаёт работать, пока не пройдёшь её в браузере. Поэтому клиент никогда не повторяет запрос после ошибки авторизации, а бот блокирует отправку до `/unlock`.

Если сертификат Jira выпущен корпоративным CA, которому система не доверяет, его PEM кладётся в хранилище (`make vault` спросит путь к файлу), и клиент доверяет только системным корням плюс этому CA. URL Jira может быть с context path: `https://example.com/jira`.

Тексты сетевых ошибок клиент пересобирает без адресов: стандартные ошибки Go содержат домен и IP, а текст уходит в Telegram и логи.

Редиректы клиент не выполняет: редирект от Jira почти всегда ведёт на страницу логина, и Basic auth за ним не отправляется. Прокси из переменных окружения игнорируются, чтобы пароль шёл только напрямую через туннель.

## Проверка доступа

```
GET /rest/api/2/myself
```

`200`, `X-Seraph-LoginReason: OK` и `X-AUSERNAME`, совпадающий с логином.

## Проверка задачи

```
GET /rest/api/2/issue/{key}?fields=summary
```

`200` - задача есть, `404` - нет или нет доступа. Ключ перед подстановкой в URL проверяется регуляркой `^[A-Z][A-Z0-9_]*-[0-9]+$`.

## Создание worklog'а

Это не REST, а форма WorklogPRO - бот отправляет её так же, как браузер. Нативный `POST /rest/api/2/issue/{key}/worklog` не подходит: он не заполняет атрибуты WorklogPRO.

```
POST /secure/WPCreateWorklogOnIssueWithWorkLogAction.jspa
Content-Type: application/x-www-form-urlencoded; charset=UTF-8
X-Requested-With: XMLHttpRequest
```

| Поле | Пример | Откуда |
|---|---|---|
| `issueKey` | `ABC-1` | пост или `JIRA_DEFAULT_ISSUE_KEY` |
| `worklogAuthorUsername` | `john.doe` | логин из хранилища |
| `timeLogged` | `8h`, `7h 30m`, `45m` | сумма записей за день |
| `startDate` | `2026-10-19 10:00` | дата + `JIRA_WORKLOG_START_TIME` |
| `startDateJS` | `19.10.26 10:00` | то же в формате формы |
| `endDate` / `endDateJS` | `2026-10-20` / `20.10.26` | следующий день, как у браузера |
| `comment` | строки записей через пустую строку | пост |
| `period` | `none` | константа |
| `adjustEstimate` | `auto` | константа |
| `remainingEstimateOptionsExpanded` | `false` | константа |
| `inline`, `decorator` | `true`, `dialog` | константа |
| `jql`, `navFilter`, `filterByProjectKey`, `commentLevel` | пусто | константа |
| `wa_*` | `wa_1=…&wa_2=…` | атрибуты WorklogPRO из хранилища, скопированы из успешного запроса браузера |

Атрибуты `wa_*` добавляются первыми, остальные поля их перекрывают: через `wa_*` нельзя подменить, например, `issueKey`.

Ответ - HTML-страница диалога, её не разбираем и никуда не пишем. Всё нужное - в заголовках:

| Заголовок | Значение |
|---|---|
| `X-Atlassian-Dialog-Control` | `DONE` - worklog создан; иначе форма вернулась с ошибкой |
| `X-Worklogpro_workloglist` | ID созданного worklog'а |
| `X-Worklogpro_periodid` | ID периода таймшита (неделя), нужен для отправки на проверку |
| `X-Worklogpro_issuelist` | ключ задачи |

Если `DONE` пришёл, а ID в заголовках нет, клиент возвращает `ErrUnknownResult`: worklog мог создаться, и бот просит проверить календарь.

## Есть ли уже worklog за дату

```
GET /rest/api/2/issue/{key}/worklog
```

Ищем запись с `author.name`, равным логину, и `started`, начинающимся с нужной даты. Так бот не дублирует дни, которые ты записал руками.

## Отправка периода на проверку

```
PUT /rest/com.deniz.jira.worklog/latest/approval/period/{periodId}/request?explanation=
```

Без тела, успех - любой `2xx`. `periodId` - из `X-Worklogpro_periodid` при создании worklog'а. Периоды недельные: если пост захватил две недели, бот отправляет оба периода.

## Календарь

```
GET /secure/WorklogPROWorklogCalendar!default.jspa
```

Ссылка на него приходит в итоговом сообщении.
