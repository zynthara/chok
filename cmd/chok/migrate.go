package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/zynthara/chok/db"
	"github.com/zynthara/chok/internal/conf"
	"github.com/zynthara/chok/internal/dbx"

	// The batteries with a schema of their own register their migrations
	// in dbx when they initialize.
	_ "github.com/zynthara/chok/identity"
	_ "github.com/zynthara/chok/identity/rbac"
	_ "github.com/zynthara/chok/jobs"
)

// The dialects a migration directory can split into.
var dialects = []string{"sqlite", "mysql", "postgres"}

// ledgerTable is the migration ledger every schema shares (plan D13).
const ledgerTable = "chok_migrations"

// migrateFlags are the flags of up, status and mark-applied.
type migrateFlags struct {
	config    string
	name      string
	dir       string
	batteries []string
}

// add registers the flags; up and status take --battery too.
func (f *migrateFlags) add(cmd *cobra.Command, battery bool) {
	cmd.Flags().StringVar(&f.config, "config", "", "configuration file (default: chok.yaml, <name>.yaml or configs/<name>.yaml, as the app finds it)")
	cmd.Flags().StringVar(&f.name, "name", "", "the app name given to chok.Load, for its <NAME>_* variables (default: the last element of ./go.mod's module path)")
	cmd.Flags().StringVar(&f.dir, "dir", "migrations", "the application's migration directory")
	if battery {
		cmd.Flags().StringSliceVar(&f.batteries, "battery", nil, "also handle this battery's migrations: identity, rbac or jobs (repeatable); those already in the ledger always are")
	}
}

func newMigrateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Create, apply and inspect versioned migrations",
		Long: `Versioned migrations are SQL files named <14-digit version>_<name>.sql,
in a directory the application embeds and passes to db.Migrations,
optionally split into sqlite/, mysql/ and postgres/. They apply once, in
version order, and every applied one is a row of the chok_migrations
ledger, which the batteries (identity, rbac, jobs) share under owners
of their own.

up, status and mark-applied read the db section of the application's
configuration the way the app does — chok.yaml and the <NAME>_DB_*
environment variables — and work on the application's owner, app, and
on the batteries the ledger already lists or --battery names.`,
	}
	cmd.AddCommand(newMigrateCreateCmd(), newMigrateUpCmd(), newMigrateStatusCmd(), newMigrateMarkAppliedCmd())
	return cmd
}

var migrationName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// versionedFile is <version>_<name>.sql, as package db reads it.
var versionedFile = regexp.MustCompile(`^(\d{14})_[A-Za-z0-9_-]+\.sql$`)

const versionLayout = "20060102150405"

func newMigrateCreateCmd() *cobra.Command {
	var dir string
	var only []string
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Write the next migration's empty SQL files",
		Long: `Write <dir>/<dialect>/<version>_<name>.sql for sqlite, mysql and
postgres — or only the dialects --dialect names — with the version the
current UTC time (later than every version already there). A directory
that keeps its files at the top, without dialect subdirectories, gets one
file at the top instead.

A file without a statement fails to load: write the SQL before running
the app or chok migrate up.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			paths, err := createMigration(dir, args[0], only, time.Now())
			for _, p := range paths {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), p)
			}
			return err
		},
	}
	cmd.Flags().StringVar(&dir, "dir", "migrations", "the application's migration directory")
	cmd.Flags().StringSliceVar(&only, "dialect", nil, "write only for these dialects: sqlite, mysql, postgres (repeatable)")
	return cmd
}

// createMigration writes the files of a new migration and returns their
// paths.
func createMigration(dir, name string, only []string, now time.Time) ([]string, error) {
	if !migrationName.MatchString(name) {
		return nil, fmt.Errorf("migrate create: %q: a migration name is letters, digits, - and _", name)
	}
	for _, d := range only {
		if !slices.Contains(dialects, d) {
			return nil, fmt.Errorf("migrate create: dialect %q: want sqlite, mysql or postgres", d)
		}
	}
	newest, flat, err := scanMigrations(dir)
	if err != nil {
		return nil, err
	}
	version := now.UTC().Truncate(time.Second)
	if v, err := time.Parse(versionLayout, newest); err == nil && !version.After(v) {
		version = v.Add(time.Second)
	}
	file := version.Format(versionLayout) + "_" + name + ".sql"

	var targets []string
	switch {
	case len(only) > 0:
		for _, d := range only {
			targets = append(targets, filepath.Join(dir, d, file))
		}
	case flat:
		targets = []string{filepath.Join(dir, file)}
	default:
		for _, d := range dialects {
			targets = append(targets, filepath.Join(dir, d, file))
		}
	}
	var written []string
	for _, p := range targets {
		dialect := filepath.Base(filepath.Dir(p))
		if !slices.Contains(dialects, dialect) {
			dialect = "every dialect"
		}
		body := fmt.Sprintf("-- %s (%s)\n--\n-- Applied once, in version order, and recorded in chok_migrations; an\n-- applied file must not change. Write the statements below, each ending\n-- with a semicolon.\n\n", strings.TrimSuffix(file, ".sql"), dialect)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return written, fmt.Errorf("migrate create: %w", err)
		}
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return written, fmt.Errorf("migrate create: %w", err)
		}
		_, werr := io.WriteString(f, body)
		if err := errors.Join(werr, f.Close()); err != nil {
			return written, fmt.Errorf("migrate create: %w", err)
		}
		written = append(written, p)
	}
	return written, nil
}

// scanMigrations finds the newest version under dir, at its top and in
// its dialect directories, and whether it keeps files at the top only.
func scanMigrations(dir string) (newest string, flat bool, err error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("migrate create: %w", err)
	}
	top, split := false, false
	look := func(name string) {
		if m := versionedFile.FindStringSubmatch(name); m != nil {
			newest = max(newest, m[1])
		}
	}
	for _, e := range entries {
		switch {
		case e.IsDir() && slices.Contains(dialects, e.Name()):
			split = true
			sub, err := os.ReadDir(filepath.Join(dir, e.Name()))
			if err != nil {
				return "", false, fmt.Errorf("migrate create: %w", err)
			}
			for _, s := range sub {
				look(s.Name())
			}
		case !e.IsDir() && strings.HasSuffix(e.Name(), ".sql"):
			top = true
			look(e.Name())
		}
	}
	return newest, top && !split, nil
}

// appDB is what the command reads of an application's configuration:
// the db section and nothing else.
type appDB struct {
	DB db.Config `yaml:"db"`
}

// loadDB reads the db section the way the application's chok.Load does:
// defaults, the file, then the <NAME>_DB_* environment variables.
func (f *migrateFlags) loadDB(stderr io.Writer) (db.Config, string, error) {
	name := f.name
	if name == "" {
		name = appName()
	}
	var c appDB
	res, err := conf.Load(&c, conf.Options{Name: name, Override: f.config, Only: []string{"db"}})
	for _, w := range res.Warnings {
		_, _ = fmt.Fprintln(stderr, "chok migrate: "+w)
	}
	if err != nil {
		return db.Config{}, "", err
	}
	return c.DB, res.File, nil
}

var moduleLine = regexp.MustCompile(`(?m)^module\s+"?([^"\s]+)"?\s*$`)

// appName is the application's name as chok init writes it: the last
// element of the module path in ./go.mod, a /vN suffix skipped, else the
// working directory's name.
func appName() string {
	if data, err := os.ReadFile("go.mod"); err == nil {
		if m := moduleLine.FindSubmatch(data); m != nil {
			parts := strings.Split(string(m[1]), "/")
			last := parts[len(parts)-1]
			if regexp.MustCompile(`^v\d+$`).MatchString(last) && len(parts) > 1 {
				last = parts[len(parts)-2]
			}
			return last
		}
	}
	wd, err := os.Getwd()
	if err != nil {
		return "app"
	}
	return filepath.Base(wd)
}

// session is one connection to the application's database with the
// schemas whose migrations the command handles registered on it.
type session struct {
	d       *db.DB
	cfg     db.Config
	file    string   // the configuration file read, "" for none
	unknown []string // ledger owners the command has no files for
}

// Which schemas a session registers, besides what the ledger says.
const (
	inUse       = ""  // the application's files, the batteries in the ledger and those --battery names
	everySchema = "*" // the application's files and every battery the command knows
)

// openSession connects to the database cfg describes, in mode, and
// registers schemas by which: inUse, everySchema, or one owner — "app"
// for the application's files alone, or a battery's name for that
// battery alone. It declares no table, so mode auto creates nothing;
// versioned applies what is pending.
func openSession(ctx context.Context, f *migrateFlags, cfg db.Config, file, mode, which string, stderr io.Writer) (*session, error) {
	for _, b := range f.batteries {
		if _, ok := dbx.Migrations[b]; !ok {
			return nil, fmt.Errorf("migrate: --battery %s: want one of %s", b, strings.Join(knownBatteries(), ", "))
		}
	}
	var app fs.FS
	if which == inUse || which == everySchema || which == "app" {
		if fi, err := os.Stat(f.dir); err == nil && fi.IsDir() {
			app = os.DirFS(f.dir)
		} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("migrate: %w", err)
		}
	}
	s := &session{cfg: cfg, file: file}
	cfg.Migrate = mode
	logger := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	d, err := db.Open(cfg, db.Migrations(app), db.Logger(logger))
	if err != nil {
		return nil, err
	}
	s.d = d
	owners, err := ledgerOwners(ctx, d)
	if err != nil {
		s.close(ctx)
		return nil, err
	}
	for _, b := range knownBatteries() {
		register := b == which || which == everySchema ||
			(which == inUse && (slices.Contains(owners, b) || slices.Contains(f.batteries, b)))
		if !register {
			continue
		}
		if err := d.RegisterSchema(b, nil, dbx.Migrations[b]); err != nil {
			s.close(ctx)
			return nil, err
		}
	}
	for _, o := range owners {
		if o != "app" && dbx.Migrations[o] == nil {
			s.unknown = append(s.unknown, o)
		}
	}
	return s, nil
}

func knownBatteries() []string {
	var out []string
	for b := range dbx.Migrations {
		out = append(out, b)
	}
	slices.Sort(out)
	return out
}

// ledgerOwners lists the owners that have rows in the ledger.
func ledgerOwners(ctx context.Context, d *db.DB) ([]string, error) {
	g := d.Unsafe(ctx)
	if !g.Migrator().HasTable(ledgerTable) {
		return nil, nil
	}
	var owners []string
	if err := g.Raw("SELECT DISTINCT owner FROM " + ledgerTable).Scan(&owners).Error; err != nil {
		return nil, fmt.Errorf("migrate: read %s: %w", ledgerTable, err)
	}
	slices.Sort(owners)
	return owners, nil
}

func (s *session) close(ctx context.Context) { _ = s.d.Close(context.WithoutCancel(ctx)) }

// statuses is MigrationStatus ordered by owner, the application first,
// then version.
func (s *session) statuses(ctx context.Context) ([]db.MigrationStatus, error) {
	sts, err := s.d.MigrationStatus(ctx)
	if err != nil {
		return nil, err
	}
	rank := func(owner string) string {
		if owner == "app" {
			return ""
		}
		return owner
	}
	slices.SortStableFunc(sts, func(a, b db.MigrationStatus) int {
		return cmp.Or(strings.Compare(rank(a.Owner), rank(b.Owner)), strings.Compare(a.Version, b.Version))
	})
	return sts, nil
}

// refuseAuto stops up and mark-applied on a database the application
// migrates itself.
func refuseAuto(f *migrateFlags, cfg db.Config, file, verb string) error {
	if cfg.Migrate != "auto" {
		return nil
	}
	where := "the configuration"
	if file != "" {
		where = file
	}
	return fmt.Errorf("migrate %s: db.migrate is auto in %s: the application creates its tables from its models and never applies %s; versioned migrations are for migrate: versioned or off", verb, where, f.dir)
}

func newMigrateStatusCmd() *cobra.Command {
	f := &migrateFlags{}
	cmd := &cobra.Command{
		Use:   "status",
		Short: "List the migrations and whether each is applied",
		Long: `List every migration of the application and of the batteries in the
ledger (or named by --battery) with its state: applied, pending,
partial (MySQL stopped after a statement; the next up resumes there),
drifted (applied, but the file changed since) or missing (applied, but
the file is gone). It changes nothing.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			cfg, file, err := f.loadDB(cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			s, err := openSession(ctx, f, cfg, file, "auto", inUse, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			defer s.close(ctx)
			sts, err := s.statuses(ctx)
			if err != nil {
				return err
			}
			return printStatus(cmd.OutOrStdout(), s, f.dir, sts)
		},
	}
	f.add(cmd, true)
	return cmd
}

func printStatus(out io.Writer, s *session, dir string, sts []db.MigrationStatus) error {
	source := "no configuration file"
	if s.file != "" {
		source = s.file
	}
	_, _ = fmt.Fprintf(out, "database: %s, migrate: %s (%s)\n\n", s.cfg.Driver, s.cfg.Migrate, source)
	if len(sts) == 0 {
		_, err := fmt.Fprintf(out, "no versioned migrations: %s has none, and the ledger lists no battery\n", dir)
		return err
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "OWNER\tVERSION\tNAME\tSTATE\tAPPLIED AT")
	for _, st := range sts {
		state, at := st.State, ""
		if st.State == "partial" {
			state = fmt.Sprintf("partial (%d statements ran)", st.Statements)
		}
		if !st.AppliedAt.IsZero() {
			at = st.AppliedAt.UTC().Format(time.RFC3339)
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", st.Owner, st.Version, st.Name, state, at)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if len(s.unknown) > 0 {
		_, _ = fmt.Fprintf(out, "\nthe ledger also lists %s, whose files this command does not have\n", strings.Join(s.unknown, ", "))
	}
	return nil
}

func newMigrateUpCmd() *cobra.Command {
	f := &migrateFlags{}
	cmd := &cobra.Command{
		Use:   "up",
		Short: "Apply every pending migration",
		Long: `Apply the pending migrations of the application and of the batteries
in the ledger or named by --battery, in version order, under the
cross-process migration lock — what the app does at start with
migrate: versioned. An application that runs with migrate: off needs it
before it starts; with migrate: auto there is nothing to apply.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			out, stderr := cmd.OutOrStdout(), cmd.ErrOrStderr()
			cfg, file, err := f.loadDB(stderr)
			if err != nil {
				return err
			}
			if err := refuseAuto(f, cfg, file, "up"); err != nil {
				return err
			}
			before, err := openSession(ctx, f, cfg, file, "auto", inUse, stderr)
			if err != nil {
				return err
			}
			was, err := before.statuses(ctx)
			before.close(ctx)
			if err != nil {
				return err
			}
			after, err := openSession(ctx, f, cfg, file, "versioned", inUse, stderr)
			if err != nil {
				return err
			}
			defer after.close(ctx)
			now, err := after.statuses(ctx)
			if err != nil {
				return err
			}
			applied := 0
			for _, st := range now {
				i := slices.IndexFunc(was, func(w db.MigrationStatus) bool { return w.Owner == st.Owner && w.Version == st.Version })
				if st.State == "applied" && (i < 0 || was[i].State != "applied") {
					applied++
					_, _ = fmt.Fprintf(out, "applied  %s  %s_%s\n", st.Owner, st.Version, st.Name)
				}
			}
			if applied == 0 {
				_, _ = fmt.Fprintln(out, "nothing to apply: every migration is applied")
			}
			return nil
		},
	}
	f.add(cmd, true)
	return cmd
}

func newMigrateMarkAppliedCmd() *cobra.Command {
	f := &migrateFlags{}
	var owner string
	cmd := &cobra.Command{
		Use:   "mark-applied <version>",
		Short: "Record a migration as applied without running it",
		Long: `Record the migration with this version as applied, without running it:
the one manual intervention, for a schema repaired by hand — after a
MySQL migration stopped halfway and its remaining statements were run
manually, say, or for tables a v2 database already has. The version is
looked up among the application's files and every battery's; only when
an application file shares its version with a battery's does --owner have
to name the one: app, or the battery.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, version := cmd.Context(), args[0]
			cfg, file, err := f.loadDB(cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			if err := refuseAuto(f, cfg, file, "mark-applied"); err != nil {
				return err
			}
			which := everySchema
			if owner != "" {
				if _, ok := dbx.Migrations[owner]; !ok && owner != "app" {
					return fmt.Errorf("migrate mark-applied: --owner %s: want app or one of %s", owner, strings.Join(knownBatteries(), ", "))
				}
				which = owner
			}
			s, err := openSession(ctx, f, cfg, file, "auto", which, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			defer s.close(ctx)
			sts, err := s.statuses(ctx)
			if err != nil {
				return err
			}
			var match []db.MigrationStatus
			for _, st := range sts {
				if st.Version == version {
					match = append(match, st)
				}
			}
			if len(match) > 1 {
				var owners []string
				for _, m := range match {
					owners = append(owners, m.Owner)
				}
				return fmt.Errorf("migrate mark-applied: version %s belongs to %s; name one with --owner", version, strings.Join(owners, " and "))
			}
			if err := s.d.MarkApplied(ctx, version); err != nil {
				return err
			}
			for _, st := range match {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "marked  %s  %s_%s  applied, without running it (it was %s)\n", st.Owner, st.Version, st.Name, st.State)
			}
			return nil
		},
	}
	f.add(cmd, false)
	cmd.Flags().StringVar(&owner, "owner", "", "the schema the version belongs to, when more than one has it: app, identity, rbac or jobs")
	return cmd
}
