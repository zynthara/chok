// Command chok is the chok v3 command line. It scaffolds a project, runs
// the versioned migrations of an application's database, saves a running
// application's OpenAPI document and reports its own build:
//
//	chok init <name>                       a project that serves on go run .
//	chok migrate create|up|status|mark-applied
//	chok openapi export <url> [--out file]
//	chok version
//
// A project made by chok init names this command in a tool directive of
// its go.mod, so `go tool chok ...` runs the release the project uses.
package main

import (
	"os"

	"github.com/spf13/cobra"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "chok",
		Short:         "The chok v3 command line",
		Version:       currentBuild().String(),
		SilenceUsage:  true,
		SilenceErrors: false,
	}
	// --version prints the same line as chok version.
	root.SetVersionTemplate("{{.Version}}\n")
	root.CompletionOptions.DisableDefaultCmd = true
	root.AddCommand(newVersionCmd())
	return root
}
