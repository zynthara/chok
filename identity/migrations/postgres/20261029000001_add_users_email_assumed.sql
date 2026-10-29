-- identity: users.email_assumed, an address only a provider's word backs.
ALTER TABLE users ADD COLUMN email_assumed boolean NOT NULL DEFAULT false;
