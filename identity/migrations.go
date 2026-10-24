package identity

import "embed"

// migrationFiles are identity's SQL migrations for migrate: versioned,
// under migrations/{sqlite,mysql,postgres}: the users, identities and
// audit_logs tables as migrate: auto creates them.
//
//go:embed migrations
var migrationFiles embed.FS
