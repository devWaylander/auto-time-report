-- migrate:up
ALTER TABLE week_posts ADD COLUMN week_start TEXT
    CHECK (date(week_start, 'weekday 1') IS week_start);

CREATE UNIQUE INDEX week_posts_user_week_start ON week_posts (user_id, week_start);

-- migrate:down
DROP INDEX week_posts_user_week_start;
ALTER TABLE week_posts DROP COLUMN week_start;
