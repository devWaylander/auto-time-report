-- migrate:up

-- Флаг manual: worklog нашёлся в Jira перед записью, но создавал его не бот (например, поставлен руками).
-- Для таких записей duration_seconds - сколько стоит в Jira на момент проверки, а периода WorklogPRO
-- у нас нет, поэтому worklog_id и period_id становятся необязательными. Последний CHECK требует их
-- у записей бота. SQLite не меняет ограничения существующей таблицы, отсюда пересборка

CREATE TABLE submissions_new (
    id               INTEGER PRIMARY KEY,
    user_id          INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    week_post_id     INTEGER REFERENCES week_posts (id) ON DELETE SET NULL,
    issue_key        TEXT    NOT NULL CHECK (issue_key GLOB '[A-Z]*-[0-9]*' AND issue_key NOT GLOB '*[^A-Z0-9_-]*'),
    work_date        TEXT    NOT NULL CHECK (date(work_date) IS work_date),
    duration_seconds INTEGER NOT NULL CHECK (duration_seconds > 0 AND duration_seconds <= 86400),
    comment          TEXT    NOT NULL,
    manual           INTEGER NOT NULL DEFAULT 0 CHECK (manual IN (0, 1)),
    -- X-Worklogpro_workloglist из ответа WorklogPRO
    worklog_id       INTEGER CHECK (worklog_id > 0),
    -- X-Worklogpro_periodid из ответа WorklogPRO
    period_id        INTEGER CHECK (period_id > 0),
    created_at       TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
        CHECK (strftime('%Y-%m-%dT%H:%M:%SZ', created_at) IS created_at),
    UNIQUE (user_id, issue_key, work_date),
    CHECK (manual = 1 OR (worklog_id IS NOT NULL AND period_id IS NOT NULL))
) STRICT;

INSERT INTO submissions_new
    (id, user_id, week_post_id, issue_key, work_date, duration_seconds, comment, worklog_id, period_id, created_at)
SELECT id, user_id, week_post_id, issue_key, work_date, duration_seconds, comment, worklog_id, period_id, created_at
FROM submissions;

DROP TABLE submissions;
ALTER TABLE submissions_new RENAME TO submissions;
CREATE INDEX submissions_user_date ON submissions (user_id, work_date);

-- migrate:down

-- Найденные в Jira записи в старой схеме не выразить: без них бот при следующей отправке найдёт их заново
DELETE FROM submissions WHERE manual = 1;

CREATE TABLE submissions_old (
    id               INTEGER PRIMARY KEY,
    user_id          INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    week_post_id     INTEGER REFERENCES week_posts (id) ON DELETE SET NULL,
    issue_key        TEXT    NOT NULL CHECK (issue_key GLOB '[A-Z]*-[0-9]*' AND issue_key NOT GLOB '*[^A-Z0-9_-]*'),
    work_date        TEXT    NOT NULL CHECK (date(work_date) IS work_date),
    duration_seconds INTEGER NOT NULL CHECK (duration_seconds > 0 AND duration_seconds <= 86400),
    comment          TEXT    NOT NULL,
    worklog_id       INTEGER NOT NULL CHECK (worklog_id > 0),
    period_id        INTEGER NOT NULL CHECK (period_id > 0),
    created_at       TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
        CHECK (strftime('%Y-%m-%dT%H:%M:%SZ', created_at) IS created_at),
    UNIQUE (user_id, issue_key, work_date)
) STRICT;

INSERT INTO submissions_old
    (id, user_id, week_post_id, issue_key, work_date, duration_seconds, comment, worklog_id, period_id, created_at)
SELECT id, user_id, week_post_id, issue_key, work_date, duration_seconds, comment, worklog_id, period_id, created_at
FROM submissions;

DROP TABLE submissions;
ALTER TABLE submissions_old RENAME TO submissions;
CREATE INDEX submissions_user_date ON submissions (user_id, work_date);
