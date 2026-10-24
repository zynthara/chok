-- jobs: the messages Enqueue writes, and each worker's watermark.
CREATE TABLE `chok_jobs_messages` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `created_at` datetime(3) NOT NULL,
  `topic` varchar(200) NOT NULL,
  `payload` longblob,
  PRIMARY KEY (`id`),
  KEY `idx_chok_jobs_messages_scan` (`created_at`,`id`),
  KEY `idx_chok_jobs_messages_topic` (`topic`,`created_at`,`id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
CREATE TABLE `chok_jobs_watermarks` (
  `topic` varchar(200) NOT NULL,
  `watermark_at` datetime(3) NOT NULL,
  `watermark_id` bigint unsigned NOT NULL,
  `updated_at` datetime(3) NOT NULL,
  PRIMARY KEY (`topic`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
