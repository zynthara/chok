package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/zynthara/chok/v2/internal/docgen"
)

// chok docs gen — the drift killer (SPEC §7.4): components tables,
// configuration reference and JSON Schema are rendered from
// Descriptor + Options and written into their homes. --check turns
// any divergence into a non-zero exit (the CI gate).

func docsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "docs",
		Short: "Documentation generators",
	}
	cmd.AddCommand(docsGenCmd())
	return cmd
}

func docsGenCmd() *cobra.Command {
	var (
		root  string
		check bool
	)
	cmd := &cobra.Command{
		Use:   "gen",
		Short: "Generate the components tables, config reference and JSON Schema",
		Long: `Generate every derived documentation surface:

  README.md / README_zh.md   <!-- gen:components --> block
  docs/design.md             <!-- gen:components --> block (Chinese)
  docs/config.md             configuration reference (whole file)
  docs/chok.schema.json      JSON Schema for chok.yaml (whole file)

With --check nothing is written; any file that would change makes the
command exit non-zero (CI drift gate).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDocsGen(cmd, root, check)
		},
	}
	cmd.Flags().StringVar(&root, "root", ".", "repository root (the chok checkout to write into)")
	cmd.Flags().BoolVar(&check, "check", false, "verify generated docs are current; non-zero exit on drift")
	return cmd
}

func runDocsGen(cmd *cobra.Command, root string, check bool) error {
	schema, err := docgen.JSONSchema()
	if err != nil {
		return err
	}

	// Whole-file surfaces.
	outputs := map[string][]byte{
		filepath.Join(root, "docs", "config.md"):        []byte(docgen.ConfigReference()),
		filepath.Join(root, "docs", "chok.schema.json"): append(schema, '\n'),
	}
	// Marker-block surfaces.
	blocks := []struct {
		path string
		lang string
	}{
		{filepath.Join(root, "README.md"), "en"},
		{filepath.Join(root, "README_zh.md"), "zh"},
		{filepath.Join(root, "docs", "design.md"), "zh"},
	}
	for _, b := range blocks {
		raw, err := os.ReadFile(b.path)
		if err != nil {
			return fmt.Errorf("docs gen: %w", err)
		}
		out, err := docgen.InjectBlock(string(raw), "components", docgen.ComponentsTable(b.lang))
		if err != nil {
			return fmt.Errorf("docs gen: %s: %w", b.path, err)
		}
		outputs[b.path] = []byte(out)
	}

	drift := false
	for path, want := range outputs {
		have, err := os.ReadFile(path)
		current := err == nil && string(have) == string(want)
		if current {
			continue
		}
		if check {
			drift = true
			fmt.Fprintf(cmd.ErrOrStderr(), "docs gen --check: %s is stale\n", path)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, want, 0o644); err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), path, "written")
	}
	if check {
		if drift {
			return fmt.Errorf("docs gen --check: generated docs are stale — run `chok docs gen`")
		}
		fmt.Fprintln(cmd.OutOrStdout(), "generated docs are current")
	}
	return nil
}
