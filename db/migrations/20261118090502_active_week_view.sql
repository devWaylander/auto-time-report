-- migrate:up
CREATE TABLE active_week_views (
    user_id    INTEGER PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    message_id INTEGER NOT NULL CHECK (message_id > 0),
    post_id    INTEGER REFERENCES week_posts (id) ON DELETE SET NULL,
    week_start TEXT    NOT NULL CHECK (date(week_start, 'weekday 1') IS week_start),
    problems   TEXT    NOT NULL,
    submit     INTEGER NOT NULL CHECK (submit IN (0, 1)),
    header     TEXT    NOT NULL
) STRICT;

-- migrate:down
DROP TABLE active_week_views;
