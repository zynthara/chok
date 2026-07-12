CREATE TABLE users (
  id bigserial PRIMARY KEY,
  rid varchar(24) NOT NULL,
  version bigint NOT NULL DEFAULT 1,
  created_at timestamptz,
  updated_at timestamptz,
  deleted_at timestamptz,
  delete_token varchar(24) NOT NULL DEFAULT '',
  email varchar(200) NOT NULL,
  email_verified boolean NOT NULL DEFAULT false,
  password_hash varchar(128) NOT NULL,
  has_password boolean NOT NULL DEFAULT false,
  password_version bigint NOT NULL DEFAULT 0,
  name varchar(100) NOT NULL DEFAULT '',
  roles varchar(500) NOT NULL DEFAULT '',
  active boolean NOT NULL DEFAULT true
);
CREATE UNIQUE INDEX idx_users_r_id ON users (rid);
CREATE INDEX idx_users_deleted_at ON users (deleted_at);
CREATE UNIQUE INDEX uk_user_email ON users (email) WHERE deleted_at IS NULL;

CREATE TABLE identities (
  id bigserial PRIMARY KEY,
  rid varchar(24) NOT NULL,
  version bigint NOT NULL DEFAULT 1,
  created_at timestamptz,
  updated_at timestamptz,
  deleted_at timestamptz,
  delete_token varchar(24) NOT NULL DEFAULT '',
  user_id varchar(32) NOT NULL,
  provider varchar(32) NOT NULL,
  provider_account_id varchar(200) NOT NULL,
  email varchar(200) NOT NULL DEFAULT '',
  profile jsonb,
  last_used_at timestamptz
);
CREATE UNIQUE INDEX idx_identities_r_id ON identities (rid);
CREATE INDEX idx_identities_deleted_at ON identities (deleted_at);
CREATE INDEX idx_identities_user_id ON identities (user_id);
CREATE INDEX ix_identity_user_provider ON identities (provider);
CREATE UNIQUE INDEX uk_identity_provider ON identities (provider, provider_account_id) WHERE deleted_at IS NULL;
