package store

import (
	"context"

	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/store/where"
)

// Pluck projects a single declared field from the rows matching the
// where options and returns the raw column values. The field resolves
// against the query allowlist exactly like WithFilter/WithOrder — an
// undeclared field is rejected, so a Store can never leak a column
// (password_hash, owner_id, ...) it does not expose for querying. The
// Store's scopes and soft-delete rules apply: callers only ever pluck
// from rows they could List.
//
// Pluck is a free function because Go methods cannot introduce type
// parameters (same reason chok.Get is one); F is the column's Go type
// and T is inferred from the store:
//
//	urls, err := store.Pluck[string](ctx, covers, "cover_url",
//	    where.WithFilterOp("cover_url", where.Ne, ""))
//
// Note that the public "id" field resolves to the rid column (the
// standing alias db.Model ships): Pluck[string](ctx, s, "id") yields
// public RIDs. Internal numeric keys are deliberately not reachable by
// field name — project them with PluckIDs.
//
// Zero matches return an empty (non-nil) slice.
func Pluck[F any, T db.Modeler](ctx context.Context, s *Store[T], field string, opts ...where.Option) ([]F, error) {
	return pluckInternal[F](ctx, s, field, false, opts)
}

// PluckIDs projects the internal numeric primary keys of the rows
// matching the where options — the inverse of ListByIDs, and the first
// half of the two-step IN pattern for cross-table reads over numeric
// foreign keys, which keeps both stores' allowlists in force where a
// hand-written JOIN through Unsafe would bypass them:
//
//	ids, _ := store.PluckIDs(ctx, sources, where.WithFilter("enabled", true))
//	page, _ := books.List(ctx, where.WithFilterIn("source_id", ids))
//
// Key sets larger than where.MaxInList ride ListIn, which chunks the IN
// list automatically under the same read semantics.
//
// Like ListByIDs, this is server-side plumbing: the numeric keys never
// belong in API responses (the public identifier is the RID, which the
// "id" field of Pluck already resolves to).
func PluckIDs[T db.Modeler](ctx context.Context, s *Store[T], opts ...where.Option) ([]uint, error) {
	return pluckColumn[uint](ctx, s, "id", false, opts)
}

// PluckDistinct is Pluck with SELECT DISTINCT on the projected column.
// When combined with WithOrder, order by the plucked field itself —
// ordering a DISTINCT projection by a different column is rejected by
// PostgreSQL (the expression must appear in the select list) and
// silently driver-defined elsewhere.
func PluckDistinct[F any, T db.Modeler](ctx context.Context, s *Store[T], field string, opts ...where.Option) ([]F, error) {
	return pluckInternal[F](ctx, s, field, true, opts)
}

func pluckInternal[F any, T db.Modeler](ctx context.Context, s *Store[T], field string, distinct bool, opts []where.Option) ([]F, error) {
	col, err := where.ResolveField(s.queryFieldMap, field)
	if err != nil {
		return nil, mapQueryError(err)
	}
	return pluckColumn[F](ctx, s, col, distinct, opts)
}

// pluckColumn runs the projection over an already-validated column:
// pluckInternal resolves it through the query allowlist, PluckIDs pins
// the fixed primary-key column.
func pluckColumn[F any, T db.Modeler](ctx context.Context, s *Store[T], col string, distinct bool, opts []where.Option) ([]F, error) {
	// Same cap discipline as listInternal: the per-Store max page size is
	// prepended so caller options appearing later can only tighten it.
	if s.maxPageSize > 0 {
		opts = append([]where.Option{where.WithMaxPageSize(s.maxPageSize)}, opts...)
	}

	q, err := s.applyScopes(ctx, s.effectiveDB(ctx).Model(new(T)))
	if err != nil {
		return nil, err
	}
	q, _, err = where.Apply(q, s.queryFieldMap, opts)
	if err != nil {
		return nil, mapQueryError(err)
	}
	if distinct {
		q = q.Distinct()
	}

	out := []F{}
	if err := q.Pluck(col, &out).Error; err != nil {
		return nil, mapError(err)
	}
	return out, nil
}
