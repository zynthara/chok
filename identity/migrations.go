package identity

import (
	"embed"

	"github.com/zynthara/chok/internal/dbx"
)

// migrationFiles are identity's SQL migrations for migrate: versioned,
// under migrations/{sqlite,mysql,postgres}: the users, identities,
// audit_logs and auth_login_codes tables as migrate: auto creates them.
//
//go:embed migrations
var migrationFiles embed.FS

// The chok command reports and applies identity's migrations through
// dbx, for an application that runs with migrate: off.
func init() { dbx.Migrations["identity"] = migrations() }
