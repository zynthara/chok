package main

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/zynthara/chok/version"
)

func main() {
	root := &cobra.Command{
		Use:     "chok",
		Short:   "chok CLI — scaffold and manage chok projects",
		Version: version.Get().String(),
	}
	// `chok --version` prints the same compact line as `chok version`
	// (no cobra-default "chok version X.Y.Z" prefix).
	root.SetVersionTemplate("{{.Version}}\n")

	root.AddCommand(initCmd())
	root.AddCommand(versionCmd())
	root.AddCommand(updateCmd())

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}
