package cache

import (
	"context"
	"time"

	"github.com/dgraph-io/badger/v4"

	"github.com/zynthara/chok/log"
)

type fileCache struct {
	db         *badger.DB
	defaultTTL time.Duration
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

func (f *fileCache) Get(_ context.Context, key string) ([]byte, bool, error) {
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

func (f *fileCache) Set(_ context.Context, key string, value []byte, ttl time.Duration) error {
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

func (f *fileCache) Delete(_ context.Context, key string) error {
	err := f.db.Update(func(txn *badger.Txn) error {
		return txn.Delete([]byte(key))
	})
	if err == badger.ErrKeyNotFound {
		return nil
	}
	return err
}

func (f *fileCache) Close() error {
	// Run a single GC pass to reclaim space from expired/deleted entries.
	_ = f.db.RunValueLogGC(0.5)
	return f.db.Close()
}
