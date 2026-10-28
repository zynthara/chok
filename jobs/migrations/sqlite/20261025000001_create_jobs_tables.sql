-- jobs: the messages Enqueue writes, each worker's watermark, the dead
-- letters, and the delivered sets a graceful stop keeps.
CREATE TABLE `chok_jobs_messages` (`id` integer PRIMARY KEY AUTOINCREMENT,`created_at` datetime NOT NULL,`topic` text NOT NULL,`payload` blob,`attempts` integer NOT NULL DEFAULT 0,`retry_at` datetime,`last_error` text);
CREATE INDEX `idx_chok_jobs_messages_scan` ON `chok_jobs_messages`(`created_at`,`id`);
CREATE INDEX `idx_chok_jobs_messages_topic` ON `chok_jobs_messages`(`topic`,`created_at`,`id`);
CREATE TABLE `chok_jobs_watermarks` (`topic` text,`watermark_at` datetime NOT NULL,`watermark_id` integer NOT NULL,`updated_at` datetime NOT NULL,PRIMARY KEY (`topic`));
CREATE TABLE `chok_jobs_dead` (`id` integer,`topic` text NOT NULL,`payload` blob,`created_at` datetime NOT NULL,`parked_at` datetime NOT NULL,`attempts` integer NOT NULL,`last_error` text,PRIMARY KEY (`id`));
CREATE INDEX `idx_chok_jobs_dead_parked` ON `chok_jobs_dead`(`parked_at`,`id`);
CREATE INDEX `idx_chok_jobs_dead_topic` ON `chok_jobs_dead`(`topic`,`parked_at`,`id`);
CREATE TABLE `chok_jobs_delivered` (`topic` text,`message_id` integer,`created_at` datetime NOT NULL,PRIMARY KEY (`topic`,`message_id`));
