CREATE TABLE casbin_rule (
  id bigserial PRIMARY KEY,
  ptype varchar(100),
  v0 varchar(100),
  v1 varchar(100),
  v2 varchar(100),
  v3 varchar(100),
  v4 varchar(100),
  v5 varchar(100)
);
CREATE UNIQUE INDEX unique_index ON casbin_rule (ptype, v0, v1, v2, v3, v4, v5);
