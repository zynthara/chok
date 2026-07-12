CREATE TABLE `users` (`id` integer PRIMARY KEY AUTOINCREMENT,`rid` text NOT NULL,`version` integer NOT NULL DEFAULT 1,`created_at` datetime,`updated_at` datetime,`deleted_at` datetime,`delete_token` text NOT NULL DEFAULT "",`email` text NOT NULL,`email_verified` numeric NOT NULL DEFAULT false,`password_hash` text NOT NULL,`has_password` numeric NOT NULL DEFAULT false,`password_version` integer NOT NULL DEFAULT 0,`name` text NOT NULL DEFAULT "",`roles` text NOT NULL DEFAULT "",`active` numeric NOT NULL DEFAULT true);
CREATE UNIQUE INDEX `idx_users_r_id` ON `users`(`rid`);
CREATE INDEX `idx_users_deleted_at` ON `users`(`deleted_at`);
CREATE UNIQUE INDEX "uk_user_email" ON "users" ("email", "delete_token");

CREATE TABLE `identities` (`id` integer PRIMARY KEY AUTOINCREMENT,`rid` text NOT NULL,`version` integer NOT NULL DEFAULT 1,`created_at` datetime,`updated_at` datetime,`deleted_at` datetime,`delete_token` text NOT NULL DEFAULT "",`user_id` text NOT NULL,`provider` text NOT NULL,`provider_account_id` text NOT NULL,`email` text NOT NULL DEFAULT "",`profile` JSON,`last_used_at` datetime);
CREATE UNIQUE INDEX `idx_identities_r_id` ON `identities`(`rid`);
CREATE INDEX `idx_identities_deleted_at` ON `identities`(`deleted_at`);
CREATE INDEX `idx_identities_user_id` ON `identities`(`user_id`);
CREATE INDEX `ix_identity_user_provider` ON `identities`(`provider`);
CREATE UNIQUE INDEX "uk_identity_provider" ON "identities" ("provider", "provider_account_id", "delete_token");
