package store

import (
	"context"
	"net/url"

	"github.com/zynthara/chok/db"
	"github.com/zynthara/chok/store/where"
)

// Reader is a read-only view of a Store. Business code that only queries
// data should depend on this interface rather than the full *Store[T].
type Reader[T db.Modeler] interface {
	Get(ctx context.Context, by Locator, opts ...QueryOption) (*T, error)
	List(ctx context.Context, opts ...where.Option) (*Page[T], error)
	ListFromQuery(ctx context.Context, query url.Values) ([]T, int64, error)
	ListByIDs(ctx context.Context, ids []uint) ([]T, error)
	Exists(ctx context.Context, by Locator) (bool, error)
}

// Writer is a write-only view of a Store. Business code that only mutates
// data should depend on this interface rather than the full *Store[T].
type Writer[T db.Modeler] interface {
	Create(ctx context.Context, obj *T) error
	Update(ctx context.Context, by Locator, changes Changes, opts ...UpdateOption) error
	Delete(ctx context.Context, by Locator, opts ...DeleteOption) error
	BatchCreate(ctx context.Context, objs []*T) error
	Upsert(ctx context.Context, obj *T, conflictColumns []string, updateColumns ...string) error
}

// ReadWriter combines Reader and Writer into a single interface covering
// standard CRUD operations without escape hatches (DB/ScopedDB/Tx).
type ReadWriter[T db.Modeler] interface {
	Reader[T]
	Writer[T]
}
