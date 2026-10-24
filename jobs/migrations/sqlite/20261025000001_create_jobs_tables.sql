-- jobs: the messages Enqueue writes, and each worker's watermark.
CREATE TABLE `chok_jobs_messages` (`id` integer PRIMARY KEY AUTOINCREMENT,`created_at` datetime NOT NULL,`topic` text NOT NULL,`payload` blob);
CREATE INDEX `idx_chok_jobs_messages_scan` ON `chok_jobs_messages`(`created_at`,`id`);
CREATE INDEX `idx_chok_jobs_messages_topic` ON `chok_jobs_messages`(`topic`,`created_at`,`id`);
CREATE TABLE `chok_jobs_watermarks` (`topic` text,`watermark_at` datetime NOT NULL,`watermark_id` integer NOT NULL,`updated_at` datetime NOT NULL,PRIMARY KEY (`topic`));
