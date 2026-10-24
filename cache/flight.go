package cache

import (
	"bytes"
	"context"
	"errors"
	"sync"
)

// flight makes concurrent loads of one key share a single call, as
// golang.org/x/sync/singleflight does, except that a waiter whose context
// ends stops waiting.
type flight struct {
	mu    sync.Mutex
	calls map[string]*call
}

type call struct {
	done chan struct{}
	val  []byte
	err  error
}

var errLoadPanicked = errors.New("cache: the load function panicked")

// do runs fn for key unless a call for key is in flight, in which case it
// waits for that call's result, a copy of its value.
func (f *flight) do(ctx context.Context, key string, fn func() ([]byte, error)) ([]byte, error) {
	f.mu.Lock()
	if c, ok := f.calls[key]; ok {
		f.mu.Unlock()
		select {
		case <-c.done:
			return bytes.Clone(c.val), c.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.calls == nil {
		f.calls = map[string]*call{}
	}
	c := &call{done: make(chan struct{}), err: errLoadPanicked}
	f.calls[key] = c
	f.mu.Unlock()
	defer func() { // also when fn panics: the waiters get errLoadPanicked
		f.mu.Lock()
		delete(f.calls, key)
		f.mu.Unlock()
		close(c.done)
	}()
	c.val, c.err = fn()
	return c.val, c.err
}
