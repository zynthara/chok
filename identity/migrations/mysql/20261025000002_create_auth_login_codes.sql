-- identity: the codes and magic-link tokens of passwordless sign-in, as HMACs.
CREATE TABLE `auth_login_codes` (
  `id` varchar(24) NOT NULL,
  `email` varchar(200) NOT NULL,
  `code_hash` varchar(64) NOT NULL,
  `token_hash` varchar(64) NOT NULL,
  `attempts` bigint NOT NULL DEFAULT '0',
  `expires_at` datetime(3) NOT NULL,
  `created_at` datetime(3) NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_login_code_email` (`email`),
  UNIQUE KEY `uk_login_code_token` (`token_hash`),
  KEY `idx_auth_login_codes_expires_at` (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
