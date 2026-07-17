package store

import (
	"context"
	"fmt"
	"reflect"
	"slices"

	"gorm.io/gorm"

	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/store/where"
)

// ListIn retrieves every row whose field value is in values — the second
// half of the two-step IN pattern (a Pluck / PluckIDs projection being
// the first), with the where.MaxInList ceiling handled by the framework:
// the value set is deduplicated, split into IN lists of at most
// where.MaxInList values, and queried chunk by chunk. Small sets execute
// as the single List(where.WithFilterIn(...)) they are equivalent to.
//
// The anchor is one big IN's SET semantics, which Go-side value dedup
// alone cannot deliver: database equality can be wider than Go equality
// (case-insensitive collations make "a" and "A" one value), so when the
// set spans multiple chunks the combined rows are additionally
// deduplicated by primary key — a row matching values in two chunks is
// returned once, exactly as a single IN would return it.
//
// Multiple chunks are multiple SELECTs, not one statement: under
// concurrent writes the combined result is not a single-snapshot read
// (a row updated between chunks may be observed in the first-seen
// state or not at all). When cross-chunk consistency matters, run
// ListIn inside a transaction (Store.Tx / db.RunInTx) and let the
// database's transaction isolation provide the snapshot.
//
// The field resolves through the query allowlist exactly like
// WithFilterIn, and every chunk runs under the Store's scopes and
// soft-delete rules — the read semantics are List's, never wider:
//
//	ids, _ := store.PluckIDs(ctx, sources, where.WithFilter("enabled", true))
//	books, _ := store.ListIn(ctx, books, "source_id", ids)
//
// Additional opts may add FILTERS ONLY, and they apply to every chunk
// query (a custom where.Option must tolerate being applied once per
// chunk). Ordering, pagination, count and page-size-cap options are
// rejected as invalid: each would hold per chunk but silently not
// compose across chunks — a per-chunk ORDER BY is not a global order,
// and a per-chunk LIMIT (including the Store's max-page-size cap, which
// ListIn deliberately bypasses) would drop rows mid-set while looking
// complete. Consequently the result carries no order guarantee; sort
// in memory, or stay under where.MaxInList and use List with
// WithFilterIn plus WithOrder.
//
// Like ListByIDs, this is server-side plumbing sized by the value set:
// results are not page-capped, and a low-cardinality field (a status
// column, say) can match far more rows than values — point ListIn at
// key-shaped fields (unique keys, foreign keys). It is a free function
// because Go methods cannot introduce type parameters; V is the field's
// Go type and T is inferred from the store.
//
// Zero values return an empty (non-nil) slice — through one degenerate
// no-match query, so the field still resolves against the allowlist,
// the guard still vets the options, and fail-closed scopes still run:
// an empty input must not hide a typo'd field name or turn an
// unauthenticated rejection into a silent empty page. This matches
// List(where.WithFilterIn(field)) over an empty set exactly.
func ListIn[T db.Modeler, V comparable](ctx context.Context, s *Store[T], field string, values []V, opts ...where.Option) ([]T, error) {
	runChunk := func(chunk []V) ([]T, error) {
		chunkOpts := make([]where.Option, 0, len(opts)+2)
		chunkOpts = append(chunkOpts, opts...)
		// The guard runs after the caller options (Config then reflects
		// them alone) and before the chunk's own IN filter. Zero means no
		// Store max-page-size injection: size discipline here is the value
		// set, and a silent per-chunk LIMIT would drop rows mid-set.
		chunkOpts = append(chunkOpts, listInFilterOnlyGuard(), where.WithFilterIn(field, chunk))
		page, err := s.listInternalWithMaxPageSize(ctx, nil, chunkOpts, 0)
		if err != nil {
			return nil, err
		}
		return page.Items, nil
	}

	unique := dedupInValues(values)
	out := []T{}
	if len(unique) == 0 {
		// The degenerate pass (WHERE 1=0) exists for its validation, not
		// its rows — allowlist, guard and scopes all run, same as List
		// over an empty WithFilterIn.
		if _, err := runChunk(nil); err != nil {
			return nil, err
		}
		return out, nil
	}
	multiChunk := len(unique) > where.MaxInList
	var seenPK map[uint]struct{}
	if multiChunk {
		seenPK = make(map[uint]struct{}, len(unique))
	}
	for chunk := range slices.Chunk(unique, where.MaxInList) {
		items, err := runChunk(chunk)
		if err != nil {
			return nil, err
		}
		if !multiChunk {
			return items, nil
		}
		// Cross-chunk row dedup keys on the internal primary key — the
		// one identity that can never collide (the RID column is unique
		// but a row backfilled outside the store could carry an empty
		// one). Value dedup above cannot do this job: database equality
		// may be wider than Go equality (case-insensitive collations),
		// so the same row can legitimately match values in two chunks.
		for _, item := range items {
			pk, err := listInPrimaryKey(s, item)
			if err != nil {
				return nil, err
			}
			if _, dup := seenPK[pk]; dup {
				continue
			}
			seenPK[pk] = struct{}{}
			out = append(out, item)
		}
	}
	return out, nil
}

// listInPrimaryKey extracts the row's internal numeric primary key via
// the GORM schema — db.Model guarantees the uint ID column, and
// store.New refuses models that fail ValidateModel, so a miss here is a
// server-side invariant break worth a loud error over a silent
// mis-dedup.
func listInPrimaryKey[T db.Modeler](s *Store[T], item T) (uint, error) {
	if s.modelSchema == nil || s.modelSchema.PrioritizedPrimaryField == nil {
		return 0, fmt.Errorf("store: ListIn: model schema carries no primary key field")
	}
	val, _ := s.modelSchema.PrioritizedPrimaryField.ValueOf(context.Background(), reflect.ValueOf(item))
	pk, ok := val.(uint)
	if !ok {
		return 0, fmt.Errorf("store: ListIn: primary key is %T, expected the db.Model uint ID", val)
	}
	return pk, nil
}

// dedupInValues collapses duplicate IN values while preserving first-seen
// order. A single IN list is set-shaped — the database returns a matching
// row once no matter how often the value repeats — and chunking must not
// change that: the same value landing in two chunks would return its rows
// twice. This is the Go-equality half; ListIn's cross-chunk primary-key
// dedup covers values the DATABASE considers equal but Go does not.
func dedupInValues[V comparable](values []V) []V {
	if len(values) <= 1 {
		return values
	}
	seen := make(map[V]struct{}, len(values))
	out := make([]V, 0, len(values))
	for _, v := range values {
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

// listInFilterOnlyGuard enforces ListIn's FILTERS-ONLY contract, the
// chunked-read sibling of cursorFilterOnlyGuard: ordering, pagination,
// count and page-size caps each hold within one chunk but silently fail
// to compose across chunks, which reads as a complete result while rows
// are missing or mis-ordered.
func listInFilterOnlyGuard() where.Option {
	return func(db *gorm.DB, cfg *where.Config, _ map[string]string) (*gorm.DB, error) {
		if cfg.HasPage || cfg.HasCursor || cfg.HasOrder || cfg.Count || cfg.MaxPageSize > 0 {
			return nil, fmt.Errorf("%w: ListIn accepts filter options only; ordering, pagination, count and page-size caps do not compose across IN chunks", where.ErrInvalidParam)
		}
		return db, nil
	}
}
