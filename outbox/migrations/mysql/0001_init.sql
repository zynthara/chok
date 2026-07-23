CREATE TABLE `outbox_messages` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `created_at` datetime(3) DEFAULT NULL,
  `topic` varchar(200) NOT NULL,
  `payload` longblob,
  PRIMARY KEY (`id`),
  KEY `idx_outbox_messages_scan` (`created_at`,`id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE `outbox_relay_state` (
  `relay_name` varchar(128) NOT NULL,
  `watermark_at` datetime(3) NOT NULL,
  `watermark_id` bigint unsigned NOT NULL,
  `updated_at` datetime(3) DEFAULT NULL,
  PRIMARY KEY (`relay_name`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
