package main

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/zynthara/chok/version"
)

func versionCmd() *cobra.Command {
	var (
		asJSON bool
		short  bool
	)
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print version and build metadata for the chok CLI",
		Long: `Print the chok CLI version, git commit hash, build time,
and Go runtime version.

  chok version           # single human-readable line
  chok version --short   # version string only (for scripts)
  chok version --json    # JSON payload (for CI / tooling)

Resolution priority: ldflags injected by ` + "`make build`" + ` /
goreleaser > debug.ReadBuildInfo (go install ...@latest) > built-in
fallback (dev / unknown).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if asJSON && short {
				return fmt.Errorf("--short and --json are mutually exclusive")
			}
			info := version.Get()
			out := cmd.OutOrStdout()
			switch {
			case asJSON:
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(info)
			case short:
				fmt.Fprintln(out, info.Version)
			default:
				fmt.Fprintln(out, info.String())
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit JSON instead of the human-readable line")
	cmd.Flags().BoolVar(&short, "short", false, "print only the version string")
	return cmd
}
