package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// chok openapi export — lands the OpenAPI spec a running app serves
// into a file (SPEC §7 item 4, roadmap Tier 3). The spec is generated
// from the route table at mount time, so the running app is the
// source of truth; the CLI's job is fetch + format, not re-deriving
// routes without the app's code.

func openapiCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "openapi",
		Short: "OpenAPI spec utilities",
	}
	cmd.AddCommand(openapiExportCmd())
	return cmd
}

func openapiExportCmd() *cobra.Command {
	var (
		addr    string
		specURL string
		out     string
		timeout time.Duration
	)
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Fetch the spec from a running app and write it to a file",
		Long: `Fetch the OpenAPI 3 spec from a running chok app and write it out.

The output format follows the file extension: .json (verbatim,
pretty-printed) or .yaml/.yml (converted). Start your app, then:

    chok openapi export --addr localhost:8080 --out openapi.yaml`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			url := specURL
			if url == "" {
				url = "http://" + addr + "/swagger/doc.json"
			}
			return runOpenAPIExport(cmd, url, out, timeout)
		},
	}
	cmd.Flags().StringVar(&addr, "addr", "localhost:8080", "host:port of the running app")
	cmd.Flags().StringVar(&specURL, "url", "", "full spec URL (overrides --addr; use when the spec path is customized)")
	cmd.Flags().StringVar(&out, "out", "openapi.json", "output file (.json, .yaml or .yml)")
	cmd.Flags().DurationVar(&timeout, "timeout", 5*time.Second, "HTTP timeout")
	return cmd
}

func runOpenAPIExport(cmd *cobra.Command, url, out string, timeout time.Duration) error {
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("openapi export: fetch %s: %w (is the app running? swagger.enabled true?)", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("openapi export: %s returned %d (swagger disabled or a custom prefix? try --url)", url, resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	var spec map[string]any
	if err := json.Unmarshal(raw, &spec); err != nil {
		return fmt.Errorf("openapi export: %s did not return a JSON spec: %w", url, err)
	}

	var rendered []byte
	switch {
	case strings.HasSuffix(out, ".yaml") || strings.HasSuffix(out, ".yml"):
		rendered, err = yaml.Marshal(spec)
	default:
		rendered, err = json.MarshalIndent(spec, "", "  ")
		rendered = append(rendered, '\n')
	}
	if err != nil {
		return err
	}
	if err := os.WriteFile(out, rendered, 0o644); err != nil {
		return err
	}
	title, version := specIdent(spec)
	fmt.Fprintf(cmd.OutOrStdout(), "%s written (%s %s, %d paths)\n", out, title, version, len(specPaths(spec)))
	return nil
}

func specIdent(spec map[string]any) (title, version string) {
	info, _ := spec["info"].(map[string]any)
	title, _ = info["title"].(string)
	version, _ = info["version"].(string)
	if title == "" {
		title = "untitled"
	}
	if version == "" {
		version = "?"
	}
	return
}

func specPaths(spec map[string]any) map[string]any {
	paths, _ := spec["paths"].(map[string]any)
	return paths
}
