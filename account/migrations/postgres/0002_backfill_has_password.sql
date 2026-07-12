UPDATE users
SET has_password = TRUE
WHERE has_password = FALSE
  AND password_hash != ''
  AND deleted_at IS NULL
  AND NOT EXISTS (
    SELECT 1 FROM identities
    WHERE identities.user_id = users.rid
      AND identities.deleted_at IS NULL
  );
