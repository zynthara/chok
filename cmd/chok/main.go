package main

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/zynthara/chok/v2/version"
)

func main() {
	if err := rootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

func rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:          "chok",
		Short:        "chok CLI — scaffold and manage chok projects",
		Version:      version.Get().String(),
		SilenceUsage: true,
	}
	// `chok --version` prints the same compact line as `chok version`
	// (no cobra-default "chok version X.Y.Z" prefix).
	root.SetVersionTemplate("{{.Version}}\n")

	root.AddCommand(initCmd())
	root.AddCommand(syncCmd())
	root.AddCommand(docsCmd())
	root.AddCommand(openapiCmd())
	root.AddCommand(versionCmd())
	root.AddCommand(updateCmd())
	root.AddCommand(migrateCmd())

	return root
}
