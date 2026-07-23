CREATE TABLE `outbox_messages` (`id` integer PRIMARY KEY AUTOINCREMENT,`created_at` datetime,`topic` varchar(200) NOT NULL,`payload` blob);
CREATE INDEX idx_outbox_messages_scan ON outbox_messages (created_at, id);
CREATE TABLE `outbox_relay_state` (`relay_name` varchar(128),`watermark_at` datetime NOT NULL,`watermark_id` integer NOT NULL,`updated_at` datetime,PRIMARY KEY (`relay_name`));
