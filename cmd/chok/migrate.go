package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/zynthara/chok/v2/conf"
	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/kernel"
)

// chok migrate — the CLI face of the versioned migration engine
// (SPEC §5.3 / §7.3). create scaffolds the next NNNN_name.sql; up,
// status and repair read the db section from the project's chok.yaml through the
// same conf loader the runtime uses (defaults, validation and section
// addressing included), open the pool via db.Open and drive the
// engine over --dir as an fs.FS — the same files the app embeds.

// migrateFlags are shared by up/status/repair (create only needs --dir).
type migrateFlags struct {
	config    string
	dir       string
	instance  string
	envPrefix string
}

func migrateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Manage versioned schema migrations (create / up / status / repair)",
		Long: `Manage the forward-only versioned migration set of a chok project
(yaml: db.migrate: versioned).

Migrations are sequence-numbered SQL files (NNNN_name.sql) that the
application embeds (db.WithMigrations) and this CLI mirrors from the
--dir directory. There are no down migrations: correcting a mistake
means shipping the next forward migration.`,
	}
	cmd.AddCommand(migrateCreateCmd(), migrateUpCmd(), migrateStatusCmd(), migrateRepairCmd())
	return cmd
}

func addMigrateFlags(cmd *cobra.Command, f *migrateFlags) {
	cmd.Flags().StringVar(&f.config, "config", "chok.yaml", "path to the project's yaml config (db section)")
	cmd.Flags().StringVar(&f.dir, "dir", "migrations", "directory holding NNNN_name.sql files")
	cmd.Flags().StringVar(&f.instance, "instance", "", "named db instance (db.instances.<name>); empty = default")
	cmd.Flags().StringVar(&f.envPrefix, "env-prefix", "",
		"honour environment overrides with this prefix (the runtime uses the upper-cased app name); empty disables env overrides")
}

// openFromConfig loads the db section exactly like the runtime
// (defaults → file → optional env) and opens a handle.
func openFromConfig(f *migrateFlags) (*db.DB, error) {
	prefix := strings.ToUpper(f.envPrefix)
	if prefix == "" {
		// Fail-closed: the CLI cannot know the app's env prefix, and
		// binding bare names would let an ambient DB_PASSWORD silently
		// override the file. An unguessable sentinel keeps the loader
		// stack intact while making real matches impossible.
		prefix = "CHOKMIGRATEENVOFF"
	}
	loader := conf.NewLoader("chok", prefix)
	loader.SetPath(f.config)

	sectionKey := kernel.SectionKeyOf(kernel.Descriptor{
		Kind: "db", Instance: f.instance, ConfigKey: "db",
	})
	if err := loader.Register(sectionKey, db.Options{}); err != nil {
		return nil, err
	}
	store, err := conf.NewStore(loader)
	if err != nil {
		return nil, err
	}
	var opts db.Options
	if err := store.Snapshot().Section(sectionKey, &opts); err != nil {
		return nil, err
	}
	if !opts.Enabled {
		return nil, fmt.Errorf("section %s has enabled: false", sectionKey)
	}
	return db.Open(opts)
}

var createNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_]*$`)

func migrateCreateCmd() *cobra.Command {
	var dir string
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Scaffold the next NNNN_<name>.sql migration file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if !createNameRe.MatchString(name) {
				return fmt.Errorf("migration name must be snake_case ([a-z0-9_], leading alnum), got %q", name)
			}
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
			next, err := nextMigrationVersion(dir)
			if err != nil {
				return err
			}
			file := filepath.Join(dir, fmt.Sprintf("%04d_%s.sql", next, name))
			skeleton := fmt.Sprintf(
				"-- %04d_%s.sql\n-- Forward-only migration (chok migrate up applies it exactly once).\n-- One statement per semicolon; dollar-quote complex bodies ($$ ... $$).\n\n",
				next, name)
			if err := os.WriteFile(file, []byte(skeleton), 0o644); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), file)
			return nil
		},
	}
	cmd.Flags().StringVar(&dir, "dir", "migrations", "directory holding NNNN_name.sql files")
	return cmd
}

// nextMigrationVersion is max(existing)+1, starting at 1. It reuses
// the engine's parser so create fail-fasts on the same stray files up
// would reject.
func nextMigrationVersion(dir string) (int64, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	var max int64
	re := regexp.MustCompile(`^(\d+)_.+\.sql$`)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if m := re.FindStringSubmatch(e.Name()); m != nil {
			v, err := strconv.ParseInt(m[1], 10, 64)
			if err == nil && v > max {
				max = v
			}
		}
	}
	return max + 1, nil
}

func migrateUpCmd() *cobra.Command {
	f := &migrateFlags{}
	cmd := &cobra.Command{
		Use:   "up",
		Short: "Apply every pending migration under the cross-process lock",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			h, err := openFromConfig(f)
			if err != nil {
				return err
			}
			defer h.Close()
			report, err := db.ApplyMigrationsWithReport(cmd.Context(), h, os.DirFS(f.dir))
			for _, a := range report.Adopted {
				fmt.Fprintf(cmd.OutOrStdout(), "adopted  %04d_%s  checksum=%s\n", a.Version, a.Name, a.Checksum)
			}
			for _, m := range report.Applied {
				fmt.Fprintf(cmd.OutOrStdout(), "applied  %04d_%s.sql\n", m.Version, m.Name)
			}
			if err != nil {
				return err
			}
			if len(report.Applied) == 0 && len(report.Adopted) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "up to date — nothing to apply")
			}
			return nil
		},
	}
	addMigrateFlags(cmd, f)
	return cmd
}

func migrateStatusCmd() *cobra.Command {
	f := &migrateFlags{}
	var check bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the complete migration audit state",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			h, err := openFromConfig(f)
			if err != nil {
				return err
			}
			defer h.Close()
			st, err := db.MigrationsStatus(cmd.Context(), h, os.DirFS(f.dir))
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			for _, a := range st.Applied {
				checksum := a.Checksum
				if checksum == "" {
					checksum = "unknown"
				}
				fmt.Fprintf(out, "applied  %04d_%s  %s  checksum=%s\n",
					a.Version, a.Name, a.AppliedAt.Format("2006-01-02 15:04:05"), checksum)
			}
			for _, p := range st.Pending {
				fmt.Fprintf(out, "pending  %04d_%s\n", p.Version, p.Name)
			}
			for _, d := range st.Dirty {
				fmt.Fprintf(out, "dirty  %04d_%s  started=%s checksum=%s error=%q\n",
					d.Version, d.Name, formatMigrationTime(d.StartedAt), d.Checksum, d.LastError)
			}
			for _, d := range st.Drift {
				fmt.Fprintf(out, "drift  %04d  file=%s ledger=%s current=%s\n",
					d.Version, d.File, d.Ledger, d.Current)
			}
			for _, m := range st.Missing {
				fmt.Fprintf(out, "missing  %04d_%s  checksum=%s\n", m.Version, m.Name, m.Checksum)
			}
			for _, u := range st.Unverified {
				fmt.Fprintf(out, "unverified  %04d_%s  run 'chok migrate up' to adopt a checksum baseline\n", u.Version, u.Name)
			}
			for _, p := range st.OutOfOrder {
				fmt.Fprintf(out, "out-of-order  %04d_%s\n", p.Version, p.Name)
			}
			for _, n := range st.NameDrift {
				fmt.Fprintf(out, "name-drift  %04d  ledger=%s current=%s file=%s\n",
					n.Version, n.LedgerName, n.FileName, n.File)
			}
			if st.Fence != nil {
				fmt.Fprintf(out, "fenced  owner=%s acquired=%s\n", st.Fence.Owner, formatMigrationTime(st.Fence.AcquiredAt))
			}
			if len(st.Applied) == 0 && len(st.Pending) == 0 && len(st.Dirty) == 0 && st.Fence == nil {
				fmt.Fprintln(out, "no migrations found")
			}
			fmt.Fprintf(out,
				"\nbuilt-in framework-owned table catalog (outside application migration history):\n  %s\n",
				strings.Join(st.FrameworkTables, ", "))
			if check && !st.Clean() {
				return fmt.Errorf("migration status is not clean")
			}
			return nil
		},
	}
	addMigrateFlags(cmd, f)
	cmd.Flags().BoolVar(&check, "check", false, "exit 1 unless there are no pending, dirty, drifted, missing, unverified, out-of-order, renamed or fenced migrations")
	return cmd
}

func formatMigrationTime(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.Format(time.RFC3339)
}

func migrateRepairCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "repair",
		Short: "Explicitly resolve one inspected dirty or drifted migration",
	}
	cmd.AddCommand(
		migrateRepairActionCmd(db.RepairRetry),
		migrateRepairActionCmd(db.RepairMarkApplied),
		migrateRepairActionCmd(db.RepairAcceptDrift),
	)
	return cmd
}

func migrateRepairActionCmd(action db.RepairAction) *cobra.Command {
	f := &migrateFlags{}
	var reason, expectedChecksum string
	short := map[db.RepairAction]string{
		db.RepairRetry:       "Clear a dirty attempt after restoring the database to its pre-migration state",
		db.RepairMarkApplied: "Mark a dirty attempt complete after verifying every intended effect exists",
		db.RepairAcceptDrift: "Accept the current file bytes as the new checksum baseline",
	}[action]
	cmd := &cobra.Command{
		Use:   string(action) + " <version>",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			version, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil || version <= 0 {
				return fmt.Errorf("migration version must be a positive integer, got %q", args[0])
			}
			h, err := openFromConfig(f)
			if err != nil {
				return err
			}
			defer h.Close()
			report, err := db.RepairMigration(cmd.Context(), h, os.DirFS(f.dir), db.RepairOptions{
				Action: action, Version: version,
				ExpectedChecksum: expectedChecksum, Reason: reason,
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"repaired action=%s version=%d file=%s ledger_checksum=%s current_checksum=%s reason=%q resolved_at=%s\n",
				report.Action, report.Version, report.File, report.LedgerChecksum,
				report.CurrentChecksum, report.Reason, report.ResolvedAt.Format(time.RFC3339))
			return nil
		},
	}
	addMigrateFlags(cmd, f)
	cmd.Flags().StringVar(&expectedChecksum, "checksum", "", "exact ledger checksum observed in status (compare-and-swap guard)")
	cmd.Flags().StringVar(&reason, "reason", "", "mandatory operational reason recorded in the repair report")
	return cmd
}
