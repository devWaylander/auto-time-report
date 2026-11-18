-- migrate:up
ALTER TABLE week_posts ADD COLUMN archive_text TEXT;
ALTER TABLE week_posts ADD COLUMN bot_button_archived INTEGER NOT NULL DEFAULT 0
    CHECK (bot_button_archived IN (0, 1));

-- migrate:down
ALTER TABLE week_posts DROP COLUMN bot_button_archived;
ALTER TABLE week_posts DROP COLUMN archive_text;
