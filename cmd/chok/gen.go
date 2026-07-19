package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/zynthara/chok/v2/internal/fieldgen"
)

// chok gen — application-side code generators (docs gen owns the
// repository-derived surfaces; sync owns the yaml-derived assembly).
// `gen fields` turns each package's store-tagged models into
// compile-checked field references so a typo'd field name fails the
// build instead of a request.

func genCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "gen",
		Short: "Code generators",
	}
	cmd.AddCommand(genFieldsCmd())
	return cmd
}

func genFieldsCmd() *cobra.Command {
	var (
		dirs  []string
		check bool
	)
	cmd := &cobra.Command{
		Use:   "fields",
		Short: "Generate " + fieldgen.GenFileName + " field references from `store` tags",
		Long: `Generate (or refresh) ` + fieldgen.GenFileName + ` next to your models.

Every top-level struct with at least one ` + "`store:`" + ` tag gets a
<Model>Fields struct variable whose values are the public field names
the store whitelists key on — WithFilter(PostFields.Title, v) instead
of WithFilter("title", v). Renaming a model field then regenerating
turns every stale call site into a compile error; the runtime
whitelists keep guarding dynamic (HTTP) field names as before.

The file is regenerated wholesale and byte-stable. A directory whose
models lost their last ` + "`store:`" + ` tag has its generated file removed.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runGenFields(cmd, dirs, check)
		},
	}
	cmd.Flags().StringArrayVar(&dirs, "dir", []string{"."}, "model package directory to scan (repeatable)")
	cmd.Flags().BoolVar(&check, "check", false, "verify generated files are current; non-zero exit on drift (CI gate)")
	return cmd
}

func runGenFields(cmd *cobra.Command, dirs []string, check bool) error {
	drift := false
	for _, dir := range dirs {
		pkg, err := fieldgen.Scan(dir)
		if err != nil {
			return err
		}
		for _, w := range pkg.Warnings {
			fmt.Fprintln(cmd.ErrOrStderr(), "gen fields: warning:", w)
		}

		outPath := filepath.Join(dir, fieldgen.GenFileName)
		existing, readErr := os.ReadFile(outPath)

		// No tagged models: the generated file must not linger as an
		// orphan referencing symbols that no longer exist.
		if len(pkg.Models) == 0 {
			switch {
			case readErr != nil:
				fmt.Fprintln(cmd.OutOrStdout(), dir, "has no store-tagged models — nothing to do")
			case check:
				drift = true
				fmt.Fprintf(cmd.ErrOrStderr(), "gen fields --check: %s is orphaned (no store-tagged models left)\n", outPath)
			default:
				if err := os.Remove(outPath); err != nil {
					return fmt.Errorf("gen fields: remove orphaned %s: %w", outPath, err)
				}
				fmt.Fprintln(cmd.OutOrStdout(), outPath, "removed (no store-tagged models left)")
			}
			continue
		}

		src, err := fieldgen.Render(pkg)
		if err != nil {
			return err
		}
		switch {
		case check:
			if readErr != nil || string(existing) != string(src) {
				drift = true
				fmt.Fprintf(cmd.ErrOrStderr(), "gen fields --check: %s is stale\n", outPath)
				continue
			}
			fmt.Fprintln(cmd.OutOrStdout(), outPath, "up to date")
		case readErr == nil && string(existing) == string(src):
			fmt.Fprintln(cmd.OutOrStdout(), outPath, "unchanged")
		default:
			if err := os.WriteFile(outPath, src, 0o644); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), outPath, "written")
		}
	}
	if drift {
		return fmt.Errorf("gen fields --check: generated field references are stale — run `chok gen fields`")
	}
	return nil
}
