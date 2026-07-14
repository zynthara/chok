package store

import (
	"context"
	"net/url"

	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/store/where"
)

// Reader is a read-only view of a Store. Business code that only queries
// data should depend on this interface rather than the full *Store[T].
type Reader[T db.Modeler] interface {
	Get(ctx context.Context, by Locator, opts ...QueryOption) (*T, error)
	List(ctx context.Context, opts ...where.Option) (*Page[T], error)
	ListFromQuery(ctx context.Context, query url.Values) ([]T, int64, where.PageInfo, error)
	ListByIDs(ctx context.Context, ids []uint) ([]T, error)
	Exists(ctx context.Context, by Locator) (bool, error)
	Count(ctx context.Context, opts ...where.Option) (int64, error)
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

// BatchWriter is the opt-in batch mutation surface. It is separate from
// Writer so adding batch capabilities does not break downstream Writer mocks
// and adapters by expanding their required method set.
type BatchWriter[T db.Modeler] interface {
	BatchCreate(ctx context.Context, objs []*T) error
	BatchUpdate(ctx context.Context, objs []*T, fields ...string) error
	BatchUpsert(ctx context.Context, objs []*T, conflictColumns []string, updateColumns ...string) error
}

// ReadWriter combines Reader and Writer into a single interface covering
// standard CRUD operations without escape hatches (DB/ScopedDB/Tx).
type ReadWriter[T db.Modeler] interface {
	Reader[T]
	Writer[T]
}
