package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/zynthara/chok/v2/conf"
	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/kernel"
)

// chok migrate — the CLI face of the versioned migration engine
// (SPEC §5.3 / §7.3). create scaffolds the next NNNN_name.sql; up and
// status read the db section from the project's chok.yaml through the
// same conf loader the runtime uses (defaults, validation and section
// addressing included), open the pool via db.Open and drive the
// engine over --dir as an fs.FS — the same files the app embeds.

// migrateFlags are shared by up/status (create only needs --dir).
type migrateFlags struct {
	config    string
	dir       string
	instance  string
	envPrefix string
}

func migrateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Manage versioned schema migrations (create / up / status)",
		Long: `Manage the forward-only versioned migration set of a chok project
(yaml: db.migrate: versioned).

Migrations are sequence-numbered SQL files (NNNN_name.sql) that the
application embeds (db.WithMigrations) and this CLI mirrors from the
--dir directory. There are no down migrations: correcting a mistake
means shipping the next forward migration.`,
	}
	cmd.AddCommand(migrateCreateCmd(), migrateUpCmd(), migrateStatusCmd())
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
			applied, err := db.ApplyMigrations(cmd.Context(), h, os.DirFS(f.dir))
			for _, m := range applied {
				fmt.Fprintf(cmd.OutOrStdout(), "applied  %04d_%s.sql\n", m.Version, m.Name)
			}
			if err != nil {
				return err
			}
			if len(applied) == 0 {
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
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show applied and pending migrations plus the framework-table whitelist",
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
				fmt.Fprintf(out, "applied  %04d_%s  %s\n",
					a.Version, a.Name, a.AppliedAt.Format("2006-01-02 15:04:05"))
			}
			for _, p := range st.Pending {
				fmt.Fprintf(out, "pending  %04d_%s\n", p.Version, p.Name)
			}
			if len(st.Applied) == 0 && len(st.Pending) == 0 {
				fmt.Fprintln(out, "no migrations found")
			}
			fmt.Fprintf(out,
				"\nframework tables (AutoMigrate-managed by chok batteries, exempt from versioned mode):\n  %s\n",
				strings.Join(st.FrameworkTables, ", "))
			return nil
		},
	}
	addMigrateFlags(cmd, f)
	return cmd
}
