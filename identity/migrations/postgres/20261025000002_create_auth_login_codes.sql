-- identity: the codes and magic-link tokens of passwordless sign-in, as HMACs.
CREATE TABLE auth_login_codes (
  id varchar(24) PRIMARY KEY,
  email varchar(200) NOT NULL,
  code_hash varchar(64) NOT NULL,
  token_hash varchar(64) NOT NULL,
  attempts bigint NOT NULL DEFAULT 0,
  expires_at timestamptz NOT NULL,
  created_at timestamptz NOT NULL
);
CREATE UNIQUE INDEX uk_login_code_email ON auth_login_codes (email);
CREATE UNIQUE INDEX uk_login_code_token ON auth_login_codes (token_hash);
CREATE INDEX idx_auth_login_codes_expires_at ON auth_login_codes (expires_at);
