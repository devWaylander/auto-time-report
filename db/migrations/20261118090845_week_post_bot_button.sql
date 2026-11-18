-- migrate:up
ALTER TABLE week_posts ADD COLUMN bot_button_base TEXT;

-- migrate:down
ALTER TABLE week_posts DROP COLUMN bot_button_base;
