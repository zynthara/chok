package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/zynthara/chok/v2/conf"
	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/internal/blessed"
	"github.com/zynthara/chok/v2/kernel"
	"github.com/zynthara/chok/v2/version"
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
		Long: `Manage the forward-only versioned migration sets of a chok project
(yaml: db.migrate: versioned).

Migrations are sequence-numbered SQL files (NNNN_name.sql) that the
application embeds (db.WithMigrations) and this CLI mirrors from the
--dir directory. Built-in account, audit and authz sequences can be applied
with up --component or up --all-owned. There are no down migrations:
correcting a mistake means shipping the next forward migration.`,
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

func ownedMigrationSequence(kind string) (db.Sequence, error) {
	sequences := blessed.MigrationSequences()
	known := make([]string, 0, len(sequences))
	for _, seq := range sequences {
		known = append(known, seq.Kind())
		if seq.Kind() == kind {
			return seq, nil
		}
	}
	return db.Sequence{}, fmt.Errorf(
		"unknown built-in migration component %q (want %s); third-party sequences are applied by the application component through db.ApplyOwnedMigrations",
		kind, strings.Join(known, "|"))
}

func renderApplyReport(out io.Writer, report *db.ApplyReport) {
	prefix := ""
	if report.Sequence != "app" {
		prefix = "[" + report.Sequence + "] "
	}
	for _, a := range report.Adopted {
		fmt.Fprintf(out, "%sadopted  %04d_%s  checksum=%s provenance=%s\n", prefix, a.Version, a.Name, a.Checksum, a.Provenance)
	}
	for _, migration := range report.Applied {
		fmt.Fprintf(out, "%sapplied  %04d_%s.sql\n", prefix, migration.Version, migration.Name)
	}
}

func renderMigrationStatus(out io.Writer, st *db.MigrationStatus) {
	fmt.Fprintf(out, "\n[%s] ledger=%s dialect=%s\n", st.Sequence, st.Ledger, st.Dialect)
	for _, a := range st.Applied {
		checksum := a.Checksum
		if checksum == "" {
			checksum = "unknown"
		}
		fmt.Fprintf(out, "applied  %04d_%s  %s  checksum=%s provenance=%s\n",
			a.Version, a.Name, a.AppliedAt.Format("2006-01-02 15:04:05"), checksum, a.Provenance)
	}
	for _, pending := range st.Pending {
		fmt.Fprintf(out, "pending  %04d_%s\n", pending.Version, pending.Name)
	}
	for _, dirty := range st.Dirty {
		fmt.Fprintf(out, "dirty  %04d_%s  started=%s checksum=%s error=%q\n",
			dirty.Version, dirty.Name, formatMigrationTime(dirty.StartedAt), dirty.Checksum, dirty.LastError)
	}
	for _, drift := range st.Drift {
		fmt.Fprintf(out, "drift  %04d  file=%s ledger=%s current=%s\n",
			drift.Version, drift.File, drift.Ledger, drift.Current)
	}
	for _, missing := range st.Missing {
		fmt.Fprintf(out, "missing  %04d_%s  checksum=%s\n", missing.Version, missing.Name, missing.Checksum)
	}
	for _, unverified := range st.Unverified {
		fmt.Fprintf(out, "unverified  %04d_%s  run 'chok migrate up' to adopt a checksum baseline\n", unverified.Version, unverified.Name)
	}
	for _, pending := range st.OutOfOrder {
		fmt.Fprintf(out, "out-of-order  %04d_%s\n", pending.Version, pending.Name)
	}
	for _, drift := range st.NameDrift {
		fmt.Fprintf(out, "name-drift  %04d  ledger=%s current=%s file=%s\n",
			drift.Version, drift.LedgerName, drift.FileName, drift.File)
	}
	if st.Fence != nil {
		fmt.Fprintf(out, "fenced  owner=%s acquired=%s\n", st.Fence.Owner, formatMigrationTime(st.Fence.AcquiredAt))
	}
	if len(st.Applied) == 0 && len(st.Pending) == 0 && len(st.Dirty) == 0 && st.Fence == nil {
		fmt.Fprintln(out, "no migrations found")
	}
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
	var component string
	var allOwned bool
	cmd := &cobra.Command{
		Use:   "up",
		Short: "Apply every pending migration under the cross-process lock",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if component != "" && allOwned {
				return fmt.Errorf("--component and --all-owned are mutually exclusive")
			}
			h, err := openFromConfig(f)
			if err != nil {
				return err
			}
			defer h.Close()
			var reports []*db.ApplyReport
			switch {
			case component != "":
				seq, seqErr := ownedMigrationSequence(component)
				if seqErr != nil {
					return seqErr
				}
				report, applyErr := db.ApplySequence(cmd.Context(), h, seq)
				reports = append(reports, report)
				if applyErr != nil {
					renderApplyReport(cmd.OutOrStdout(), report)
					return applyErr
				}
			case allOwned:
				for _, seq := range blessed.MigrationSequences() {
					report, applyErr := db.ApplySequence(cmd.Context(), h, seq)
					reports = append(reports, report)
					if applyErr != nil {
						for _, rendered := range reports {
							renderApplyReport(cmd.OutOrStdout(), rendered)
						}
						return applyErr
					}
				}
			default:
				report, applyErr := db.ApplyMigrationsWithReport(cmd.Context(), h, os.DirFS(f.dir))
				reports = append(reports, report)
				if applyErr != nil {
					renderApplyReport(cmd.OutOrStdout(), report)
					return applyErr
				}
			}
			changed := false
			for _, report := range reports {
				renderApplyReport(cmd.OutOrStdout(), report)
				changed = changed || len(report.Applied) > 0 || len(report.Adopted) > 0
			}
			if !changed {
				fmt.Fprintln(cmd.OutOrStdout(), "up to date — nothing to apply")
			}
			return nil
		},
	}
	addMigrateFlags(cmd, f)
	cmd.Flags().StringVar(&component, "component", "", "apply one built-in owned sequence (account|audit|authz)")
	cmd.Flags().BoolVar(&allOwned, "all-owned", false, "apply every built-in owned sequence")
	return cmd
}

func migrateStatusCmd() *cobra.Command {
	f := &migrateFlags{}
	var check, ledgerHealthOnly bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the complete migration audit state",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if ledgerHealthOnly && !check {
				return fmt.Errorf("--ledger-health-only requires --check")
			}
			h, err := openFromConfig(f)
			if err != nil {
				return err
			}
			defer h.Close()
			appStatus, err := db.MigrationsStatus(cmd.Context(), h, os.DirFS(f.dir))
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			info := version.Get()
			fmt.Fprintf(out, "chok=%s git=%s\n", info.Version, info.GitHash)
			statuses := []*db.MigrationStatus{appStatus}
			for _, seq := range blessed.MigrationSequences() {
				present, presentErr := db.SequencePresent(cmd.Context(), h, seq)
				if presentErr != nil {
					return presentErr
				}
				if !present {
					continue
				}
				status, statusErr := db.SequenceStatus(cmd.Context(), h, seq)
				if statusErr != nil {
					return statusErr
				}
				statuses = append(statuses, status)
			}
			for _, status := range statuses {
				renderMigrationStatus(out, status)
			}
			catalog, err := ownedMigrationCatalog(cmd.Context(), h)
			if err != nil {
				return err
			}
			renderOwnedMigrationCatalog(out, catalog)
			fmt.Fprintf(out,
				"\nbuilt-in framework-owned table catalog (outside application migration history):\n  %s\n",
				strings.Join(appStatus.FrameworkTables, ", "))
			if check {
				for _, status := range statuses {
					if !status.Clean() {
						return fmt.Errorf("migration status is not clean (sequence %s)", status.Sequence)
					}
				}
				if err := checkOwnedMigrationCatalog(catalog, ledgerHealthOnly); err != nil {
					return err
				}
			}
			return nil
		},
	}
	addMigrateFlags(cmd, f)
	cmd.Flags().BoolVar(&check, "check", false, "exit 1 unless application and owned migration state is fully verified and clean")
	cmd.Flags().BoolVar(&ledgerHealthOnly, "ledger-health-only", false, "with --check, allow unclaimed or file-unverified third-party sequences while still rejecting unhealthy ledgers and incompatible engine floors")
	return cmd
}

type ownedMigrationCatalogRow struct {
	entry    *db.ManifestEntry
	snapshot *db.SequenceLedgerSnapshot
	content  string
}

func ownedMigrationCatalog(ctx context.Context, h *db.DB) ([]ownedMigrationCatalogRow, error) {
	entries, err := db.ManifestEntries(ctx, h)
	if err != nil {
		return nil, err
	}
	byKind := make(map[string]*db.ManifestEntry, len(entries))
	kinds := make(map[string]struct{}, len(entries))
	for i := range entries {
		byKind[entries[i].Kind] = &entries[i]
		kinds[entries[i].Kind] = struct{}{}
	}
	tables, err := h.Unsafe(ctx).Migrator().GetTables()
	if err != nil {
		return nil, fmt.Errorf("inspect owned migration ledgers: %w", err)
	}
	const prefix = "schema_migrations_chok_"
	for _, table := range tables {
		if !strings.HasPrefix(table, prefix) || table == prefix+"manifest" {
			continue
		}
		kind := strings.TrimPrefix(table, prefix)
		// A table outside the documented grammar is not chok-managed merely
		// because it has a similar prefix. Valid kinds are read again below,
		// where ledger corruption must propagate rather than be mistaken for
		// an invalid identifier and hidden from status.
		if db.ValidateSequenceKind(kind) != nil {
			continue
		}
		kinds[kind] = struct{}{}
	}
	ordered := make([]string, 0, len(kinds))
	for kind := range kinds {
		ordered = append(ordered, kind)
	}
	sort.Strings(ordered)
	builtIn := make(map[string]struct{})
	for _, seq := range blessed.MigrationSequences() {
		builtIn[seq.Kind()] = struct{}{}
	}
	rows := make([]ownedMigrationCatalogRow, 0, len(ordered))
	for _, kind := range ordered {
		snapshot, err := db.LedgerSnapshot(ctx, h, kind)
		if err != nil {
			return nil, err
		}
		content := "unverified"
		if _, ok := builtIn[kind]; ok {
			content = "verified-by-binary"
		}
		rows = append(rows, ownedMigrationCatalogRow{entry: byKind[kind], snapshot: snapshot, content: content})
	}
	return rows, nil
}

func renderOwnedMigrationCatalog(out io.Writer, rows []ownedMigrationCatalogRow) {
	if len(rows) == 0 {
		return
	}
	fmt.Fprintln(out, "\nowned sequence manifest and ledger panorama:")
	for _, row := range rows {
		claim, owner, provenance := "unclaimed", "unknown", "unclaimed"
		floor, componentVersion, chokVersion := 0, "", ""
		if row.entry != nil {
			claim = "claimed"
			owner = row.entry.Owner
			provenance = row.entry.Provenance
			floor = row.entry.EngineFloor
			componentVersion = row.entry.ComponentVersion
			chokVersion = row.entry.ChokVersion
		}
		ledgerState := "missing"
		if row.snapshot.Exists {
			ledgerState = "present"
		}
		fmt.Fprintf(out,
			"[%s] ledger=%s ledger_state=%s claim=%s owner=%s provenance=%s engine_floor=%d component=%s chok=%s content=%s frontier=%d rows=%d dirty=%d unverified=%d fenced=%t\n",
			row.snapshot.Kind, row.snapshot.Ledger, ledgerState, claim, owner, provenance, floor,
			emptyMigrationMetadata(componentVersion), emptyMigrationMetadata(chokVersion), row.content,
			row.snapshot.Frontier, row.snapshot.Rows, row.snapshot.Dirty, row.snapshot.Unverified, row.snapshot.Fence != nil,
		)
	}
}

func emptyMigrationMetadata(value string) string {
	if value == "" {
		return "unknown"
	}
	return value
}

func checkOwnedMigrationCatalog(rows []ownedMigrationCatalogRow, ledgerHealthOnly bool) error {
	for _, row := range rows {
		kind := row.snapshot.Kind
		if !row.snapshot.Exists {
			return fmt.Errorf("migration status is not clean (sequence %s ledger is missing)", kind)
		}
		if row.entry != nil && !row.entry.EngineCompatible() {
			return fmt.Errorf("migration status is not clean (sequence %s requires engine generation %d; this build supports %d)", kind, row.entry.EngineFloor, db.MigrationEngineGeneration)
		}
		if row.snapshot.Dirty > 0 || row.snapshot.Fence != nil || row.snapshot.Unverified > 0 {
			return fmt.Errorf("migration status is not clean (sequence %s ledger health: dirty=%d unverified=%d fenced=%t)", kind, row.snapshot.Dirty, row.snapshot.Unverified, row.snapshot.Fence != nil)
		}
		if ledgerHealthOnly {
			continue
		}
		if row.entry == nil {
			return fmt.Errorf("migration status is not clean (sequence %s is unclaimed)", kind)
		}
		if row.content == "unverified" {
			return fmt.Errorf("migration status is not clean (sequence %s content is unavailable to this CLI binary)", kind)
		}
	}
	return nil
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
		migrateRepairClaimCmd(),
		migrateRepairHistoryCmd(),
	)
	return cmd
}

func migrateRepairHistoryCmd() *cobra.Command {
	f := &migrateFlags{}
	var kind string
	var limit int
	cmd := &cobra.Command{
		Use:   "history",
		Short: "Show persisted repair evidence, most recent first",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			h, err := openFromConfig(f)
			if err != nil {
				return err
			}
			defer h.Close()
			records, err := db.RepairHistory(cmd.Context(), h, db.RepairHistoryFilter{Kind: kind, Limit: limit})
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(records) == 0 {
				fmt.Fprintln(out, "no repair history")
				return nil
			}
			for _, record := range records {
				fmt.Fprintf(out, "[%s] %s  action=%s", record.Kind, record.RepairedAt.Format(time.RFC3339), record.Action)
				if record.Version > 0 {
					fmt.Fprintf(out, " version=%d file=%q ledger_checksum=%s current_checksum=%s",
						record.Version, record.File, record.LedgerChecksum, record.CurrentChecksum)
				}
				if record.PreviousOwner != "" || record.NewOwner != "" {
					fmt.Fprintf(out, " previous_owner=%s new_owner=%s", record.PreviousOwner, record.NewOwner)
				}
				fmt.Fprintf(out, " operator=%s chok=%s reason=%q\n",
					emptyMigrationMetadata(record.Operator), emptyMigrationMetadata(record.ChokVersion), record.Reason)
			}
			return nil
		},
	}
	addMigrateFlags(cmd, f)
	cmd.Flags().StringVar(&kind, "kind", "", "filter by kind (app = application ledger); empty = all")
	cmd.Flags().IntVar(&limit, "limit", 20, "maximum rows, most recent first")
	return cmd
}

func migrateRepairClaimCmd() *cobra.Command {
	f := &migrateFlags{}
	var kind, expectedOwner, newOwner, reason, operator string
	cmd := &cobra.Command{
		Use:   "claim",
		Short: "Transfer one inspected owned-sequence manifest claim",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if kind == "" || expectedOwner == "" || newOwner == "" || reason == "" {
				return fmt.Errorf("--kind, --expected-owner, --new-owner and --reason are required")
			}
			h, err := openFromConfig(f)
			if err != nil {
				return err
			}
			defer h.Close()
			report, err := db.RepairSequenceClaim(cmd.Context(), h, kind, db.RepairClaimOptions{
				ExpectedOwner: expectedOwner,
				NewOwner:      newOwner,
				Reason:        reason,
				Operator:      operator,
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"repaired sequence=%s ledger=%s dialect=%s previous_owner=%s new_owner=%s repaired_at=%s\n",
				report.Kind, report.Ledger, report.Dialect, report.PreviousOwner, report.NewOwner, report.RepairedAt.Format(time.RFC3339))
			return nil
		},
	}
	addMigrateFlags(cmd, f)
	cmd.Flags().StringVar(&kind, "kind", "", "owned migration kind whose existing claim is transferred")
	cmd.Flags().StringVar(&expectedOwner, "expected-owner", "", "exact current owner observed in migrate status")
	cmd.Flags().StringVar(&newOwner, "new-owner", "", "new full component import path")
	cmd.Flags().StringVar(&reason, "reason", "", "why this claim is transferred (persisted to repair history)")
	cmd.Flags().StringVar(&operator, "operator", "", "identity recorded in repair history (default: derived user@host)")
	return cmd
}

func migrateRepairActionCmd(action db.RepairAction) *cobra.Command {
	f := &migrateFlags{}
	var reason, expectedChecksum, newChecksum, component, operator string
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
			opts := db.RepairOptions{
				Action: action, Version: version,
				ExpectedChecksum: expectedChecksum, Reason: reason, Operator: operator,
			}
			var report *db.RepairReport
			if component == "" {
				if action == db.RepairAcceptDrift {
					files, loadErr := db.LoadMigrations(os.DirFS(f.dir))
					if loadErr != nil {
						return loadErr
					}
					if err := verifyNewChecksum(files, version, newChecksum); err != nil {
						return err
					}
				}
				report, err = db.RepairMigration(cmd.Context(), h, os.DirFS(f.dir), opts)
			} else {
				seq, seqErr := ownedMigrationSequence(component)
				if seqErr != nil {
					return seqErr
				}
				if action == db.RepairAcceptDrift {
					status, statusErr := db.SequenceStatus(cmd.Context(), h, seq)
					if statusErr != nil {
						return statusErr
					}
					current := ""
					for _, drift := range status.Drift {
						if drift.Version == version {
							current = drift.Current
							break
						}
					}
					if current == "" || newChecksum != current {
						return fmt.Errorf("--new-checksum must equal the current checksum shown by status for sequence %s version %d", component, version)
					}
				}
				report, err = db.RepairSequence(cmd.Context(), h, seq, opts)
			}
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"repaired sequence=%s ledger=%s dialect=%s action=%s version=%d file=%s ledger_checksum=%s current_checksum=%s reason=%q resolved_at=%s\n",
				report.Sequence, report.Ledger, report.Dialect, report.Action, report.Version, report.File, report.LedgerChecksum,
				report.CurrentChecksum, report.Reason, report.ResolvedAt.Format(time.RFC3339))
			return nil
		},
	}
	addMigrateFlags(cmd, f)
	cmd.Flags().StringVar(&expectedChecksum, "checksum", "", "exact ledger checksum observed in status (compare-and-swap guard)")
	cmd.Flags().StringVar(&newChecksum, "new-checksum", "", "current file checksum observed in status (required by accept-drift)")
	cmd.Flags().StringVar(&reason, "reason", "", "mandatory operational reason recorded in the repair report")
	cmd.Flags().StringVar(&operator, "operator", "", "identity recorded in repair history (default: derived user@host)")
	cmd.Flags().StringVar(&component, "component", "", "repair one built-in owned sequence (account|audit|authz); empty = application")
	return cmd
}

func verifyNewChecksum(files []db.Migration, version int64, checksum string) error {
	for _, file := range files {
		if file.Version == version {
			if checksum != file.Checksum {
				return fmt.Errorf("--new-checksum must equal the current checksum shown by status for version %d", version)
			}
			return nil
		}
	}
	return fmt.Errorf("migration version %d has no current file", version)
}
