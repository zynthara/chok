CREATE TABLE `casbin_rule` (`id` integer PRIMARY KEY AUTOINCREMENT,`ptype` text,`v0` text,`v1` text,`v2` text,`v3` text,`v4` text,`v5` text);
CREATE UNIQUE INDEX `unique_index` ON `casbin_rule`(`ptype`,`v0`,`v1`,`v2`,`v3`,`v4`,`v5`);
