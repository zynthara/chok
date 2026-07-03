package ctxval

import (
	"context"
	"testing"
)

func TestRoutePattern_OuterContextSeesInnerWrite(t *testing.T) {
	// The whole point of the holder: a value written at the innermost
	// layer is visible through the context captured at the outermost.
	outer, rp := WithRoutePattern(context.Background())

	inner := context.WithValue(outer, struct{ k string }{"x"}, 1) // simulate WithContext copies
	RoutePatternHolder(inner).Set("GET /posts/{rid}")

	if got := RoutePatternFrom(outer); got != "GET /posts/{rid}" {
		t.Fatalf("outer ctx must observe the inner write, got %q", got)
	}
	_ = rp
}

func TestRoutePattern_ZeroValueSemantics(t *testing.T) {
	if got := RoutePatternFrom(context.Background()); got != "" {
		t.Fatalf("no slot installed ⇒ empty, got %q", got)
	}
	if RoutePatternHolder(context.Background()) != nil {
		t.Fatal("no slot installed ⇒ nil holder")
	}
	// nil-holder Set must be a safe no-op (middleware without web root).
	RoutePatternHolder(context.Background()).Set("GET /x")

	ctx, _ := WithRoutePattern(context.Background())
	if got := RoutePatternFrom(ctx); got != "" {
		t.Fatalf("installed but unset ⇒ empty (unmatched), got %q", got)
	}
}
