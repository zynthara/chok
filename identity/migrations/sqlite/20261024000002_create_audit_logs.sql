-- identity: the append-only audit log.
CREATE TABLE `audit_logs` (`id` text,`occurred_at` datetime NOT NULL,`actor_id` text,`actor_type` text,`actor_ip` text,`action` text NOT NULL,`result` text NOT NULL,`resource` text,`resource_id` text,`before_data` JSON,`after_data` JSON,`trace_id` text,`request_id` text,`metadata` JSON,`reason` text,PRIMARY KEY (`id`));
CREATE INDEX `idx_audit_logs_occurred_at` ON `audit_logs`(`occurred_at`);
CREATE INDEX `idx_audit_actor_time` ON `audit_logs`(`actor_id`,`occurred_at`);
CREATE INDEX `idx_audit_action_time` ON `audit_logs`(`action`,`occurred_at`);
CREATE INDEX `idx_audit_resource_time` ON `audit_logs`(`resource`,`resource_id`,`occurred_at`);
CREATE INDEX `idx_audit_logs_trace_id` ON `audit_logs`(`trace_id`);
CREATE INDEX `idx_audit_logs_request_id` ON `audit_logs`(`request_id`);
