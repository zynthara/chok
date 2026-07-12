CREATE TABLE `audit_logs` (`id` varchar(24),`occurred_at` datetime NOT NULL,`actor_id` varchar(64),`actor_type` varchar(16),`actor_ip` varchar(45),`action` varchar(64),`result` varchar(16),`resource` varchar(64),`resource_id` varchar(64),`before` JSON,`after` JSON,`trace_id` varchar(32),`request_id` varchar(64),`metadata` JSON,`reason` varchar(512),PRIMARY KEY (`id`));
CREATE INDEX `idx_audit_logs_occurred_at` ON `audit_logs`(`occurred_at`);
CREATE INDEX `idx_audit_actor_time` ON `audit_logs`(`actor_id`);
CREATE INDEX `idx_audit_action_time` ON `audit_logs`(`action`);
CREATE INDEX `idx_audit_resource_time` ON `audit_logs`(`resource`,`resource_id`);
CREATE INDEX `idx_audit_logs_trace_id` ON `audit_logs`(`trace_id`);
CREATE INDEX `idx_audit_logs_request_id` ON `audit_logs`(`request_id`);
