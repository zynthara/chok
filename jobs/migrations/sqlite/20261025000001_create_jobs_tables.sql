-- jobs: the messages Enqueue writes, each worker's watermark and the dead
-- letters.
CREATE TABLE `chok_jobs_messages` (`id` integer PRIMARY KEY AUTOINCREMENT,`created_at` datetime NOT NULL,`topic` text NOT NULL,`payload` blob,`attempts` integer NOT NULL DEFAULT 0,`retry_at` datetime,`last_error` text,`completed_at` datetime);
CREATE INDEX `idx_chok_jobs_messages_scan` ON `chok_jobs_messages`(`created_at`,`id`);
CREATE INDEX `idx_chok_jobs_messages_topic` ON `chok_jobs_messages`(`topic`,`created_at`,`id`);
CREATE INDEX `idx_chok_jobs_messages_pending` ON `chok_jobs_messages`(`topic`,`completed_at`,`created_at`,`id`);
CREATE TABLE `chok_jobs_watermarks` (`topic` text,`watermark_at` datetime NOT NULL,`watermark_id` integer NOT NULL,`updated_at` datetime NOT NULL,PRIMARY KEY (`topic`));
CREATE TABLE `chok_jobs_dead` (`id` integer,`topic` text NOT NULL,`payload` blob,`created_at` datetime NOT NULL,`parked_at` datetime NOT NULL,`attempts` integer NOT NULL,`last_error` text,PRIMARY KEY (`id`));
CREATE INDEX `idx_chok_jobs_dead_parked` ON `chok_jobs_dead`(`parked_at`,`id`);
CREATE INDEX `idx_chok_jobs_dead_topic` ON `chok_jobs_dead`(`topic`,`parked_at`,`id`);
