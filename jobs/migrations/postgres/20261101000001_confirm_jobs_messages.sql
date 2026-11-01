ALTER TABLE chok_jobs_messages ADD COLUMN completed_at timestamptz;
CREATE INDEX idx_chok_jobs_messages_pending ON chok_jobs_messages (topic, completed_at, created_at, id);
UPDATE chok_jobs_messages SET completed_at = created_at WHERE id IN (SELECT id FROM chok_jobs_dead) OR id IN (SELECT message_id FROM chok_jobs_delivered);
