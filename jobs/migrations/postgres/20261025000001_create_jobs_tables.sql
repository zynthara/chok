-- jobs: the messages Enqueue writes, each worker's watermark and the dead
-- letters.
CREATE TABLE chok_jobs_messages (
  id bigserial PRIMARY KEY,
  created_at timestamptz NOT NULL,
  topic varchar(200) NOT NULL,
  payload bytea,
  attempts bigint NOT NULL DEFAULT 0,
  retry_at timestamptz,
  last_error text,
  completed_at timestamptz
);
CREATE INDEX idx_chok_jobs_messages_scan ON chok_jobs_messages (created_at, id);
CREATE INDEX idx_chok_jobs_messages_topic ON chok_jobs_messages (topic, created_at, id);
CREATE INDEX idx_chok_jobs_messages_pending ON chok_jobs_messages (topic, completed_at, created_at, id);
CREATE TABLE chok_jobs_watermarks (
  topic varchar(200) PRIMARY KEY,
  watermark_at timestamptz NOT NULL,
  watermark_id bigint NOT NULL,
  updated_at timestamptz NOT NULL
);
CREATE TABLE chok_jobs_dead (
  id bigint PRIMARY KEY,
  topic varchar(200) NOT NULL,
  payload bytea,
  created_at timestamptz NOT NULL,
  parked_at timestamptz NOT NULL,
  attempts bigint NOT NULL,
  last_error text
);
CREATE INDEX idx_chok_jobs_dead_parked ON chok_jobs_dead (parked_at, id);
CREATE INDEX idx_chok_jobs_dead_topic ON chok_jobs_dead (topic, parked_at, id);
