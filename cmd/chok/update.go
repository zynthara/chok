package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/zynthara/chok/version"
)

const (
	chokInstallPath = "github.com/zynthara/chok/cmd/chok"
	githubLatestAPI = "https://api.github.com/repos/zynthara/chok/releases/latest"
)

func updateCmd() *cobra.Command {
	var (
		ref     string
		check   bool
		dryRun  bool
		timeout time.Duration
	)
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Upgrade the local chok CLI to the latest (or pinned) version",
		Long: `Upgrade the local chok CLI. Equivalent to:
  go install ` + chokInstallPath + `@<ref>

The command shells out to the Go toolchain, so a local Go install
(>= 1.21) is required.

  chok update                  # install the latest release tag
  chok update --ref v1.2.3     # install a specific version
  chok update --ref main       # track the main branch HEAD
  chok update --check          # query the latest release without installing
  chok update --dry-run        # print the install command but do not run it

A toolchain-free upgrade path (downloading a prebuilt binary from
GitHub Releases via the selfupdate library) is on the roadmap.
The current implementation (M1) is a thin wrapper around go install.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()

			latest, err := fetchLatestRelease(timeout)
			if err != nil {
				// Network failure shouldn't block --ref or --dry-run paths.
				if check {
					return fmt.Errorf("query latest release: %w", err)
				}
				fmt.Fprintf(out, "warning: cannot query latest release (%v); proceeding with the requested ref\n", err)
			}

			current := version.Get().Version
			target := ref
			if target == "" {
				target = latest
			}
			if target == "" {
				// Latest unknown and no --ref → fall back to "latest" so
				// `go install` still does the right thing.
				target = "latest"
			}

			fmt.Fprintf(out, "current: %s\n", current)
			if latest != "" {
				fmt.Fprintf(out, "latest:  %s\n", latest)
			}
			fmt.Fprintf(out, "target:  %s\n", target)

			if check {
				if latest != "" && current == latest {
					fmt.Fprintln(out, "Already on the latest release.")
				}
				return nil
			}

			cmdLine := []string{"go", "install", chokInstallPath + "@" + target}
			if dryRun {
				fmt.Fprintln(out, "dry-run:", strings.Join(cmdLine, " "))
				return nil
			}

			fmt.Fprintln(out, "running:", strings.Join(cmdLine, " "))
			install := exec.Command(cmdLine[0], cmdLine[1:]...)
			install.Stdout = out
			install.Stderr = cmd.ErrOrStderr()
			install.Env = os.Environ()
			if err := install.Run(); err != nil {
				return fmt.Errorf("go install failed: %w", err)
			}
			fmt.Fprintln(out, "Upgrade complete. Run `chok version` to confirm.")
			return nil
		},
	}
	cmd.Flags().StringVar(&ref, "ref", "", "target ref (git tag / branch / commit); empty means latest release")
	cmd.Flags().BoolVar(&check, "check", false, "query only, do not install")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the install command without executing it")
	cmd.Flags().DurationVar(&timeout, "timeout", 5*time.Second, "timeout for the GitHub release API query")
	return cmd
}

// fetchLatestRelease queries the GitHub Releases API for the latest tag.
// Returns the empty string when offline / not authenticated for private
// repos / API unavailable — callers must treat empty as "unknown" rather
// than as a hard failure.
func fetchLatestRelease(timeout time.Duration) (string, error) {
	client := &http.Client{Timeout: timeout}
	req, err := http.NewRequest(http.MethodGet, githubLatestAPI, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "chok-cli")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		// No releases yet; treat as "unknown" rather than error so
		// `chok update --ref main` still works on a fresh repo.
		return "", nil
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github api status %d", resp.StatusCode)
	}

	var payload struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", err
	}
	return payload.TagName, nil
}
