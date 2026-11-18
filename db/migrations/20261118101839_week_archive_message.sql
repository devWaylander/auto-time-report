-- migrate:up
ALTER TABLE week_posts ADD COLUMN archive_message_id INTEGER CHECK (archive_message_id > 0);

-- migrate:down
ALTER TABLE week_posts DROP COLUMN archive_message_id;
