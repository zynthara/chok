package reqinfo

import (
	"context"
	"testing"
)

// The router writes the route into the Info an outer layer created; the
// outer layer reads it after the handler, through its own context.
func TestInfo_OuterContextSeesInnerWrite(t *testing.T) {
	outer, info := New(context.Background())
	inner := context.WithValue(outer, struct{}{}, "derived")
	From(inner).Route = "/posts/{id}"
	if info.Route != "/posts/{id}" || From(outer).Route != "/posts/{id}" {
		t.Fatalf("got %q", info.Route)
	}
}

func TestFrom_NilOutsideChok(t *testing.T) {
	if From(context.Background()) != nil {
		t.Fatal("no Info outside a chok request")
	}
	_, info := New(context.Background())
	if info.Route != "" {
		t.Fatal("a fresh Info names no route")
	}
}
