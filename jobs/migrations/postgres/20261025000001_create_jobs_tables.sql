-- jobs: the messages Enqueue writes, and each worker's watermark.
CREATE TABLE chok_jobs_messages (
  id bigserial PRIMARY KEY,
  created_at timestamptz NOT NULL,
  topic varchar(200) NOT NULL,
  payload bytea
);
CREATE INDEX idx_chok_jobs_messages_scan ON chok_jobs_messages (created_at, id);
CREATE INDEX idx_chok_jobs_messages_topic ON chok_jobs_messages (topic, created_at, id);
CREATE TABLE chok_jobs_watermarks (
  topic varchar(200) PRIMARY KEY,
  watermark_at timestamptz NOT NULL,
  watermark_id bigint NOT NULL,
  updated_at timestamptz NOT NULL
);
