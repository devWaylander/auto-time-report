-- migrate:up

-- Даты хранятся как TEXT в формате YYYY-MM-DD, моменты времени - как TEXT RFC3339 в UTC (YYYY-MM-DDTHH:MM:SSZ).
-- Формат проверяется через функции SQLite: date() и strftime() возвращают NULL для мусора и нормализуют
-- несуществующие даты (2026-02-31 -> 2026-03-03), поэтому сравнение с исходной строкой отсекает и то и другое.
-- IS вместо = пропускает NULL в необязательных полях

CREATE TABLE users (
    id          INTEGER PRIMARY KEY,
    telegram_id INTEGER NOT NULL UNIQUE,
    created_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
        CHECK (strftime('%Y-%m-%dT%H:%M:%SZ', created_at) IS created_at)
) STRICT;

-- Настройки напоминаний, одна строка на пользователя; значения по умолчанию живут здесь
CREATE TABLE user_settings (
    user_id        INTEGER PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    daily_enabled  INTEGER NOT NULL DEFAULT 1 CHECK (daily_enabled IN (0, 1)),
    daily_time     TEXT    NOT NULL DEFAULT '19:00'
        CHECK (daily_time GLOB '[01][0-9]:[0-5][0-9]' OR daily_time GLOB '2[0-3]:[0-5][0-9]'),
    weekly_enabled INTEGER NOT NULL DEFAULT 1 CHECK (weekly_enabled IN (0, 1)),
    -- 0 = воскресенье ... 6 = суббота, как time.Weekday в Go
    weekly_weekday INTEGER NOT NULL DEFAULT 5 CHECK (weekly_weekday BETWEEN 0 AND 6),
    weekly_time    TEXT    NOT NULL DEFAULT '16:00'
        CHECK (weekly_time GLOB '[01][0-9]:[0-5][0-9]' OR weekly_time GLOB '2[0-3]:[0-5][0-9]'),
    paused_until   TEXT
        CHECK (strftime('%Y-%m-%dT%H:%M:%SZ', paused_until) IS paused_until),
    updated_at     TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
        CHECK (strftime('%Y-%m-%dT%H:%M:%SZ', updated_at) IS updated_at)
) STRICT;

CREATE TABLE vacations (
    id         INTEGER PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    start_date TEXT    NOT NULL CHECK (date(start_date) IS start_date),
    end_date   TEXT    NOT NULL CHECK (date(end_date) IS end_date),
    created_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
        CHECK (strftime('%Y-%m-%dT%H:%M:%SZ', created_at) IS created_at),
    CHECK (end_date >= start_date)
) STRICT;

CREATE INDEX vacations_user_dates ON vacations (user_id, start_date, end_date);

-- Недельные посты из канала; text - последняя версия после всех правок
CREATE TABLE week_posts (
    id         INTEGER PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    chat_id    INTEGER NOT NULL,
    message_id INTEGER NOT NULL,
    text       TEXT    NOT NULL,
    posted_at  TEXT    NOT NULL CHECK (strftime('%Y-%m-%dT%H:%M:%SZ', posted_at) IS posted_at),
    edited_at  TEXT             CHECK (strftime('%Y-%m-%dT%H:%M:%SZ', edited_at) IS edited_at),
    UNIQUE (chat_id, message_id)
) STRICT;

CREATE INDEX week_posts_user_posted ON week_posts (user_id, posted_at);

-- Успешно записанные worklog'и: одна строка на пару "задача + дата"
CREATE TABLE submissions (
    id               INTEGER PRIMARY KEY,
    user_id          INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    week_post_id     INTEGER REFERENCES week_posts (id) ON DELETE SET NULL,
    issue_key        TEXT    NOT NULL CHECK (issue_key GLOB '[A-Z]*-[0-9]*' AND issue_key NOT GLOB '*[^A-Z0-9_-]*'),
    work_date        TEXT    NOT NULL CHECK (date(work_date) IS work_date),
    duration_seconds INTEGER NOT NULL CHECK (duration_seconds > 0 AND duration_seconds <= 86400),
    comment          TEXT    NOT NULL,
    -- X-Worklogpro_workloglist из ответа WorklogPRO
    worklog_id       INTEGER NOT NULL CHECK (worklog_id > 0),
    -- X-Worklogpro_periodid из ответа WorklogPRO
    period_id        INTEGER NOT NULL CHECK (period_id > 0),
    created_at       TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
        CHECK (strftime('%Y-%m-%dT%H:%M:%SZ', created_at) IS created_at),
    UNIQUE (user_id, issue_key, work_date)
) STRICT;

CREATE INDEX submissions_user_date ON submissions (user_id, work_date);

-- Периоды WorklogPRO, отправленные на проверку
CREATE TABLE period_approvals (
    id           INTEGER PRIMARY KEY,
    user_id      INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    period_id    INTEGER NOT NULL CHECK (period_id > 0),
    requested_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
        CHECK (strftime('%Y-%m-%dT%H:%M:%SZ', requested_at) IS requested_at),
    UNIQUE (user_id, period_id)
) STRICT;

-- Отправленные напоминания, чтобы после рестарта не слать их повторно
CREATE TABLE notifications (
    id          INTEGER PRIMARY KEY,
    user_id     INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    kind        TEXT    NOT NULL CHECK (kind IN ('daily', 'weekly')),
    target_date TEXT    NOT NULL CHECK (date(target_date) IS target_date),
    sent_at     TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
        CHECK (strftime('%Y-%m-%dT%H:%M:%SZ', sent_at) IS sent_at),
    UNIQUE (user_id, kind, target_date)
) STRICT;

-- migrate:down

DROP TABLE notifications;
DROP TABLE period_approvals;
DROP TABLE submissions;
DROP TABLE week_posts;
DROP TABLE vacations;
DROP TABLE user_settings;
DROP TABLE users;
