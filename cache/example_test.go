package cache_test

import (
	"context"
	"fmt"
	"time"

	"github.com/zynthara/chok/cache"
)

// GetOrLoad in front of a slow source: the first call loads and stores,
// the next is a hit; Delete invalidates. A Config from chok.Load names
// its layers in the cache section; this one is built in code.
func Example() {
	var cfg cache.Config
	cfg.Memory.Enabled = true
	c := cache.MustNew(cfg)
	defer func() { _ = c.Close(context.Background()) }()
	ctx := context.Background()

	loads := 0
	load := func(context.Context) ([]byte, error) {
		loads++
		return []byte(`{"title":"Hello chok"}`), nil
	}
	for range 2 {
		v, err := c.GetOrLoad(ctx, "post:pst_1", time.Minute, load)
		fmt.Println(string(v), err)
	}
	fmt.Println("loads:", loads)

	_ = c.Delete(ctx, "post:pst_1")
	_, hit, _ := c.Get(ctx, "post:pst_1")
	fmt.Println("hit after delete:", hit)
	// Output:
	// {"title":"Hello chok"} <nil>
	// {"title":"Hello chok"} <nil>
	// loads: 1
	// hit after delete: false
}
