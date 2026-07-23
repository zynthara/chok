CREATE TABLE outbox_messages (
  id bigserial PRIMARY KEY,
  created_at timestamptz,
  topic varchar(200) NOT NULL,
  payload bytea
);
CREATE INDEX idx_outbox_messages_scan ON outbox_messages (created_at, id);
CREATE TABLE outbox_relay_state (
  relay_name varchar(128) PRIMARY KEY,
  watermark_at timestamptz NOT NULL,
  watermark_id bigint NOT NULL,
  updated_at timestamptz
);
