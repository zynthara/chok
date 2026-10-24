// Package db opens the application's database and manages its schema.
//
//	d := db.MustOpen(cfg.DB, db.Tables(
//		db.Table(&Post{}, db.SoftUnique("uk_post_title_owner", "title", "owner_id")),
//	))
//	app.Use(d) // health, and close after everything that uses it
//
// Three dialects stand on equal footing: sqlite (pure Go), mysql and
// postgres; every battery and every test runs on all three.
//
// # Models
//
// A model embeds one of four bases: Model (a numeric key that stays
// internal, a public RID, an optimistic-lock version, timestamps),
// SoftDeleteModel, OwnedModel or OwnedSoftDeleteModel. RIDPrefixer names
// the RIDs: pst_3fK9aQ2mZx7B.
//
// # Migrations
//
// db.migrate picks one of three modes, applied at Open to the
// application's schema and at RegisterSchema to each battery's:
//
//   - auto: AutoMigrate the declared tables and create their SoftUnique
//     indexes. The default, for development and small services.
//   - versioned: apply the SQL files given to Migrations, named
//     <14-digit version>_<name>.sql, forward only. One ledger table,
//     chok_migrations, records them per owner ("app" and each battery).
//     An edited applied file is ErrChecksumDrift; MarkApplied is the one
//     manual intervention.
//   - off: touch nothing, but refuse to start (ErrNotMigrated) while a
//     versioned file is pending — for processes that should never migrate.
//
// SQLite and Postgres apply each file in one transaction. MySQL commits
// DDL implicitly, so it runs a file statement by statement and records
// each one: a file that fails halfway resumes after the last statement
// that ran, once the failing one is fixed. A Postgres advisory lock or a
// MySQL GET_LOCK keeps concurrent processes from migrating twice.
//
// # MySQL
//
// Sessions run in UTC (parseTime=true, loc=UTC, time_zone '+00:00'), so a
// time.Time stores as its UTC wall clock whatever the host's zone, and the
// zone is not configurable. Identifiers chok generates are not quoted
// against reserved words: name no column key, order or group.
//
// # Transactions
//
// RunInTx puts the transaction in the context it hands fn: stores and
// Unsafe called with that context join it, a nested RunInTx joins it, and
// AfterCommit defers work until it commits. Unsafe is the only way to the
// underlying *gorm.DB.
package db
