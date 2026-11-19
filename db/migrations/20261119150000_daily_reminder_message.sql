-- migrate:up
ALTER TABLE notifications ADD COLUMN message_id INTEGER CHECK (message_id > 0);

-- migrate:down
ALTER TABLE notifications DROP COLUMN message_id;
