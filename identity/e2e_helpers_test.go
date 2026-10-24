package identity_test

import (
	"encoding/json/v2"
	"net/http"
	"testing"
)

func decode(t *testing.T, resp *http.Response, out any) {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	if err := json.UnmarshalRead(resp.Body, out); err != nil {
		t.Fatalf("decode: %v", err)
	}
}
