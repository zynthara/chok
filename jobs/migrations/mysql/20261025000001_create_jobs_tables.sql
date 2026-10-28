-- jobs: the messages Enqueue writes, each worker's watermark, the dead
-- letters, and the delivered sets a graceful stop keeps.
CREATE TABLE `chok_jobs_messages` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `created_at` datetime(3) NOT NULL,
  `topic` varchar(200) NOT NULL,
  `payload` longblob,
  `attempts` bigint NOT NULL DEFAULT '0',
  `retry_at` datetime(3) DEFAULT NULL,
  `last_error` longtext,
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
CREATE TABLE `chok_jobs_dead` (
  `id` bigint unsigned NOT NULL,
  `topic` varchar(200) NOT NULL,
  `payload` longblob,
  `created_at` datetime(3) NOT NULL,
  `parked_at` datetime(3) NOT NULL,
  `attempts` bigint NOT NULL,
  `last_error` longtext,
  PRIMARY KEY (`id`),
  KEY `idx_chok_jobs_dead_parked` (`parked_at`,`id`),
  KEY `idx_chok_jobs_dead_topic` (`topic`,`parked_at`,`id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
CREATE TABLE `chok_jobs_delivered` (
  `topic` varchar(200) NOT NULL,
  `message_id` bigint unsigned NOT NULL,
  `created_at` datetime(3) NOT NULL,
  PRIMARY KEY (`topic`,`message_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
