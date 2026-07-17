package store

import (
	"context"

	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/store/where"
)

// Reader is a read-only view of a Store. Business code that only queries
// data should depend on this interface rather than the full *Store[T].
//
// The contract is deliberately transport-free: parsing HTTP query strings
// into query options happens at the edges (handler.HandleList, or the
// *Store.ListFromQuery sugar), never inside the data interface.
type Reader[T db.Modeler] interface {
	Get(ctx context.Context, by Locator, opts ...QueryOption) (*T, error)
	List(ctx context.Context, opts ...where.Option) (*Page[T], error)
	ListByIDs(ctx context.Context, ids []uint) ([]T, error)
	Exists(ctx context.Context, by Locator) (bool, error)
	Count(ctx context.Context, opts ...where.Option) (int64, error)
}

// Writer is the single-row write view of a Store. Business code that only
// mutates data should depend on this interface rather than the full
// *Store[T]. Multi-row mutations live on BatchWriter, so growing the batch
// surface never expands the method set Writer mocks and adapters must
// implement.
type Writer[T db.Modeler] interface {
	Create(ctx context.Context, obj *T) error
	Update(ctx context.Context, by Locator, changes Changes, opts ...UpdateOption) error
	Upsert(ctx context.Context, obj *T, conflictColumns []string, updateColumns ...string) error
	Delete(ctx context.Context, by Locator, opts ...DeleteOption) error
}

// BatchWriter is the opt-in multi-row mutation surface, kept separate from
// Writer along the single-row/batch line so each view stays minimal.
type BatchWriter[T db.Modeler] interface {
	BatchCreate(ctx context.Context, objs []*T) error
	BatchUpdate(ctx context.Context, objs []*T, fields ...string) error
	BatchUpsert(ctx context.Context, objs []*T, conflictColumns []string, updateColumns ...string) error
}

// ReadWriter combines Reader and Writer into a single interface covering
// standard single-row CRUD without escape hatches (DB/ScopedDB/Tx); batch
// mutations stay on BatchWriter.
type ReadWriter[T db.Modeler] interface {
	Reader[T]
	Writer[T]
}
