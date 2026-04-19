package cache

import (
	"context"
	"sync"
	"time"

	"github.com/dgraph-io/badger/v4"

	"github.com/zynthara/chok/log"
)

type fileCache struct {
	db         *badger.DB
	defaultTTL time.Duration
	closeOnce  sync.Once
	closeErr   error
}

// NewFile creates a file-based cache backed by badger.
// logger is optional; if nil, badger logs are discarded.
func NewFile(opts *FileOptions, logger ...log.Logger) (Cache, error) {
	bopts := badger.DefaultOptions(opts.Path)
	if len(logger) > 0 && logger[0] != nil {
		bopts.Logger = &badgerLogger{l: logger[0]}
	} else {
		bopts.Logger = nil // discard badger's verbose default logging
	}

	db, err := badger.Open(bopts)
	if err != nil {
		return nil, err
	}
	return &fileCache{db: db, defaultTTL: opts.TTL}, nil
}

func (f *fileCache) Get(ctx context.Context, key string) ([]byte, bool, error) {
	// Badger View/Update APIs don't natively accept context. Check at the
	// boundary so an already-cancelled request doesn't pay for the IO.
	// Longer-running transactions still finish once they start, which is
	// the same semantics as the Redis driver's pre-send check.
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	var val []byte
	err := f.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get([]byte(key))
		if err != nil {
			return err
		}
		val, err = item.ValueCopy(nil)
		return err
	})
	if err == badger.ErrKeyNotFound {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return val, true, nil
}

func (f *fileCache) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if ttl == 0 {
		ttl = f.defaultTTL
	}
	return f.db.Update(func(txn *badger.Txn) error {
		e := badger.NewEntry([]byte(key), value)
		if ttl > 0 {
			e = e.WithTTL(ttl)
		}
		return txn.SetEntry(e)
	})
}

func (f *fileCache) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	err := f.db.Update(func(txn *badger.Txn) error {
		return txn.Delete([]byte(key))
	})
	if err == badger.ErrKeyNotFound {
		return nil
	}
	return err
}

func (f *fileCache) Close() error {
	f.closeOnce.Do(func() {
		// Run a single GC pass in a bounded goroutine so a slow
		// compaction can't block shutdown past the caller's budget.
		// Under load shutdown, skipping GC is preferable to overshooting
		// the container's terminationGracePeriod — the next Open will
		// compact anyway.
		gcDone := make(chan struct{})
		go func() {
			defer close(gcDone)
			_ = f.db.RunValueLogGC(0.5)
		}()
		select {
		case <-gcDone:
		case <-time.After(2 * time.Second):
			// GC still running; let it finish in the background. The
			// subsequent Close below will serialise with it.
		}
		f.closeErr = f.db.Close()
	})
	return f.closeErr
}
