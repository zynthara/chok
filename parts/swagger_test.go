package parts

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func init() { gin.SetMode(gin.TestMode) }

func TestSwaggerComponent_Disabled_NoOp(t *testing.T) {
	c := NewSwaggerComponent(func(any) *SwaggerSettings {
		return &SwaggerSettings{Enabled: false}
	})
	if err := c.Init(context.Background(), newMockKernel(nil)); err != nil {
		t.Fatal(err)
	}
	if c.Spec() != nil {
		t.Fatal("Spec() should be nil when disabled")
	}

	// Mount should be a safe no-op.
	r := gin.New()
	if err := c.Mount(r); err != nil {
		t.Fatal(err)
	}
}

func TestSwaggerComponent_Enabled_MountsSpec(t *testing.T) {
	c := NewSwaggerComponent(func(any) *SwaggerSettings {
		return &SwaggerSettings{
			Enabled: true,
			Title:   "Test API",
			Version: "9.9.9",
			Prefix:  "/swagger",
		}
	})
	if err := c.Init(context.Background(), newMockKernel(nil)); err != nil {
		t.Fatal(err)
	}
	if c.Spec() == nil {
		t.Fatal("Spec() should be non-nil when enabled")
	}

	r := gin.New()
	if err := c.Mount(r); err != nil {
		t.Fatal(err)
	}

	// Verify the spec endpoint is reachable.
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/swagger/swagger.json", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 on /swagger/swagger.json, got %d", w.Code)
	}
}

func TestSwaggerComponent_Mount_RejectsBadRouter(t *testing.T) {
	c := NewSwaggerComponent(func(any) *SwaggerSettings {
		return &SwaggerSettings{Enabled: true, Title: "x", Version: "1"}
	})
	if err := c.Init(context.Background(), newMockKernel(nil)); err != nil {
		t.Fatal(err)
	}
	if err := c.Mount("not a router"); err == nil {
		t.Fatal("Mount should reject non-router argument")
	}
}

func TestSwaggerComponent_NilSettings_Disabled(t *testing.T) {
	c := NewSwaggerComponent(func(any) *SwaggerSettings { return nil })
	if err := c.Init(context.Background(), newMockKernel(nil)); err != nil {
		t.Fatal(err)
	}
	if c.Spec() != nil {
		t.Fatal("nil settings should disable the component")
	}
}
