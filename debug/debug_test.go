package debug_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zynthara/chok/v2/choktest"
	"github.com/zynthara/chok/v2/debug"
)

func TestDebug_DisabledByDefault(t *testing.T) {
	tk := choktest.NewTestKernel(t, "", debug.Module())
	if _, ok := tk.Router.Handler(http.MethodGet, "/componentz"); ok {
		t.Fatal("debug defaults to disabled — nothing must mount")
	}
	// Disabled is visible, not vanished (SPEC §3.1 definition 3).
	for _, s := range tk.Components() {
		if s.Key.Kind == "debug" && s.State != "disabled" {
			t.Fatalf("debug must report disabled, got %s", s.State)
		}
	}
}

func TestDebug_ComponentzFromDescriptors(t *testing.T) {
	tk := choktest.NewTestKernel(t, `
debug:
  enabled: true
`, debug.Module())

	h, ok := tk.Router.Handler(http.MethodGet, "/componentz")
	if !ok {
		t.Fatalf("componentz must mount when enabled: %v", tk.Router.Patterns())
	}

	// Event delivery is async (bus): poll until the init lines land.
	deadline := time.Now().Add(5 * time.Second)
	for {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/componentz", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("componentz: %d", w.Code)
		}
		var resp struct {
			Components []struct {
				Component string `json:"component"`
				State     string `json:"state"`
			} `json:"components"`
			Events []struct {
				Event string `json:"event"`
			} `json:"events"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}

		hasDebug := false
		for _, c := range resp.Components {
			if c.Component == "debug" && c.State == "ready" {
				hasDebug = true
			}
		}
		gotInitEvent := false
		for _, e := range resp.Events {
			if strings.HasPrefix(e.Event, "initialized ") {
				gotInitEvent = true
			}
		}
		if hasDebug && gotInitEvent {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("componentz incomplete: %s", w.Body.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}
