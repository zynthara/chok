package swagger

import "fmt"

// swaggerHTML returns an HTML page that loads Swagger UI from CDN.
func swaggerHTML(specURL string) string {
	return fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <title>Swagger UI</title>
  <link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5/swagger-ui.css">
</head>
<body>
  <div id="swagger-ui"></div>
  <script src="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5/swagger-ui-bundle.js"></script>
  <script>
    SwaggerUIBundle({ url: %q, dom_id: '#swagger-ui', presets: [SwaggerUIBundle.presets.apis] });
  </script>
</body>
</html>`, specURL)
}
