package store

import (
	"context"
	"fmt"
	"slices"

	"gorm.io/gorm"

	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/store/where"
)

// ListIn retrieves every row whose field value is in values — the second
// half of the two-step IN pattern (a Pluck / PluckIDs projection being
// the first), with the where.MaxInList ceiling handled by the framework:
// the value set is deduplicated (IN is set-shaped; a value repeated
// across chunk boundaries must not duplicate its row), split into IN
// lists of at most where.MaxInList values, and queried chunk by chunk.
// Small sets execute as the single List(where.WithFilterIn(...)) they
// are equivalent to.
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
// Zero values return an empty (non-nil) slice without querying.
func ListIn[T db.Modeler, V comparable](ctx context.Context, s *Store[T], field string, values []V, opts ...where.Option) ([]T, error) {
	unique := dedupInValues(values)
	out := []T{}
	for chunk := range slices.Chunk(unique, where.MaxInList) {
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
		out = append(out, page.Items...)
	}
	return out, nil
}

// dedupInValues collapses duplicate IN values while preserving first-seen
// order. A single IN list is set-shaped — the database returns a matching
// row once no matter how often the value repeats — and chunking must not
// change that: the same value landing in two chunks would return its rows
// twice.
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
