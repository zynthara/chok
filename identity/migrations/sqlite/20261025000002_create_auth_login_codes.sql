-- identity: the codes and magic-link tokens of passwordless sign-in, as HMACs.
CREATE TABLE `auth_login_codes` (`id` text,`email` text NOT NULL,`code_hash` text NOT NULL,`token_hash` text NOT NULL,`attempts` integer NOT NULL DEFAULT 0,`expires_at` datetime NOT NULL,`created_at` datetime NOT NULL,PRIMARY KEY (`id`));
CREATE INDEX `idx_auth_login_codes_expires_at` ON `auth_login_codes`(`expires_at`);
CREATE UNIQUE INDEX `uk_login_code_token` ON `auth_login_codes`(`token_hash`);
CREATE UNIQUE INDEX `uk_login_code_email` ON `auth_login_codes`(`email`);
