package main

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"
)

// defaultOpenAPIPath is http.openapi.path's default.
const defaultOpenAPIPath = "/openapi.json"

func newOpenAPICmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "openapi",
		Short: "Work with the OpenAPI document of a running application",
	}
	cmd.AddCommand(newOpenAPIExportCmd())
	return cmd
}

func newOpenAPIExportCmd() *cobra.Command {
	var out string
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "export <url>",
		Short: "Save the OpenAPI document a running application serves",
		Long: `Fetch the OpenAPI document of a running chok application and print it,
or write it to --out. <url> is the document's address, or the
application's address alone, which gets the default path /openapi.json;
without a scheme it is http:

    chok openapi export localhost:8080 --out openapi.json
    chok openapi export https://api.example.com/openapi.json --out api.yaml

The response must be an OpenAPI document in JSON. --out writes it as
JSON, or as YAML when the name ends in .yaml or .yml. An existing --out
file is replaced, and only once the document has been fetched and
checked: a failed export leaves the file as it was.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()
			doc, err := fetchOpenAPI(ctx, args[0])
			if err != nil {
				return err
			}
			yamlOut := strings.HasSuffix(out, ".yaml") || strings.HasSuffix(out, ".yml")
			rendered, err := renderOpenAPI(doc, yamlOut)
			if err != nil {
				return err
			}
			if out == "" {
				_, err := cmd.OutOrStdout().Write(rendered)
				return err
			}
			if err := writeFileAtomic(out, rendered); err != nil {
				return fmt.Errorf("openapi export: %w", err)
			}
			_, err = fmt.Fprintf(cmd.ErrOrStderr(), "%s: %s\n", out, describeOpenAPI(doc))
			return err
		},
	}
	cmd.Flags().StringVarP(&out, "out", "o", "", "write the document to this file (.json, .yaml or .yml) instead of stdout")
	cmd.Flags().DurationVar(&timeout, "timeout", 10*time.Second, "how long to wait for the application")
	return cmd
}

// openAPIURL completes what the user typed: http when there is no
// scheme, the default document path when there is no path.
func openAPIURL(raw string) (string, error) {
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("openapi export: %q is not an http(s) address", raw)
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = defaultOpenAPIPath
	}
	return u.String(), nil
}

// fetchOpenAPI gets the document and checks that it is OpenAPI JSON.
func fetchOpenAPI(ctx context.Context, raw string) (jsontext.Value, error) {
	target, err := openAPIURL(raw)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("openapi export: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openapi export: is the application running? %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("openapi export: GET %s: %w", target, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openapi export: GET %s: %s (is http.openapi.enabled off, or http.openapi.path not %s? give the document's full address)",
			target, resp.Status, defaultOpenAPIPath)
	}
	var head struct {
		OpenAPI string `json:"openapi"`
	}
	if err := json.Unmarshal(body, &head, json.RejectUnknownMembers(false)); err != nil || head.OpenAPI == "" {
		return nil, fmt.Errorf("openapi export: GET %s did not return an OpenAPI document in JSON", target)
	}
	return jsontext.Value(body), nil
}

// renderOpenAPI indents the document as JSON, keeping its key order, or
// converts it to YAML in the same order.
func renderOpenAPI(doc jsontext.Value, asYAML bool) ([]byte, error) {
	if !asYAML {
		v := bytes.Clone(doc)
		val := jsontext.Value(v)
		if err := val.Format(jsontext.Multiline(true), jsontext.WithIndent("  ")); err != nil {
			return nil, err
		}
		return append([]byte(val), '\n'), nil
	}
	// JSON is YAML: decoding it into a node keeps the order of every
	// mapping. Block style reads better than the flow style it arrives in;
	// the encoder still quotes a string that would read as another type.
	var node yaml.Node
	if err := yaml.Unmarshal(doc, &node); err != nil {
		return nil, err
	}
	blockStyle(&node)
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&node); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func blockStyle(n *yaml.Node) {
	n.Style &^= yaml.FlowStyle | yaml.DoubleQuotedStyle
	for _, c := range n.Content {
		blockStyle(c)
	}
}

// describeOpenAPI is the one line export prints about a written document.
func describeOpenAPI(doc jsontext.Value) string {
	var d struct {
		OpenAPI string `json:"openapi"`
		Info    struct {
			Title   string `json:"title"`
			Version string `json:"version"`
		} `json:"info"`
		Paths map[string]jsontext.Value `json:"paths"`
	}
	_ = json.Unmarshal(doc, &d)
	return fmt.Sprintf("OpenAPI %s, %q %s, %d paths", d.OpenAPI, d.Info.Title, d.Info.Version, len(d.Paths))
}

// writeFileAtomic replaces path with data: written to a temporary file in
// the same directory, then renamed over it.
func writeFileAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	_, werr := f.Write(data)
	if err := errors.Join(werr, f.Chmod(0o644), f.Close()); err != nil {
		_ = os.Remove(f.Name())
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		_ = os.Remove(f.Name())
		return err
	}
	return nil
}
