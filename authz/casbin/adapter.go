package casbin

import (
	"errors"
	"fmt"

	"github.com/casbin/casbin/v3/model"
	"github.com/casbin/casbin/v3/persist"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// CasbinRule is the chok-shipped storage row for Casbin policies.
// Wire-compatible with gorm-adapter v3's table layout (table name +
// columns + sizes + composite unique index named "unique_index"), so a
// deployment that has already run with gorm-adapter v3 can switch to
// the chok adapter without migration. The unique index spans Ptype +
// V0..V5 so the database itself rejects duplicate (ptype, rule) tuples
// — Casbin's in-memory model dedupes per-instance, but multi-instance
// Bootstrap or operators bypassing the Service path would otherwise
// leave duplicate rows behind.
//
// We pull our own adapter rather than depending on gorm-adapter v3
// because gorm-adapter blank-imports gorm.io/driver/postgres,
// gorm.io/driver/sqlserver (and pulls jackc/pgx-v5 +
// microsoft/go-mssqldb + glebarez/sqlite + modernc/sqlite as
// transitives) from its own init code. Those drivers cost +8.72 MB
// stripped on darwin/arm64 even when the chok app uses neither
// Postgres nor SQL Server. Empirical measurement (see Phase 6
// follow-up commit message): the chok adapter brings the same Casbin
// runtime down to +1.21 MB above the no-authz baseline.
//
// The adapter delegates to the same *gorm.DB chok's domain models
// already use, so it works on whichever driver the application
// configured (gorm.io/driver/sqlite, /mysql, or any future driver
// the operator pulls in via blank import). gorm.AutoMigrate handles
// the table creation portably.
type CasbinRule struct {
	ID    uint   `gorm:"primaryKey;autoIncrement"`
	Ptype string `gorm:"size:100;uniqueIndex:unique_index"`
	V0    string `gorm:"size:100;uniqueIndex:unique_index"`
	V1    string `gorm:"size:100;uniqueIndex:unique_index"`
	V2    string `gorm:"size:100;uniqueIndex:unique_index"`
	V3    string `gorm:"size:100;uniqueIndex:unique_index"`
	V4    string `gorm:"size:100;uniqueIndex:unique_index"`
	V5    string `gorm:"size:100;uniqueIndex:unique_index"`
}

// TableName pins the storage name to "casbin_rule" so existing data
// from gorm-adapter v3 deployments stays accessible.
func (CasbinRule) TableName() string { return "casbin_rule" }

// maxRuleColumns is the storage width of CasbinRule (V0..V5). Custom
// Options.Model that defines policy with more than 6 fields cannot be
// persisted by this adapter; ruleToRow rejects them at the boundary.
const maxRuleColumns = 6

// gormAdapter is chok's persist.Adapter implementation. It satisfies
// Casbin's persist.Adapter interface (LoadPolicy / SavePolicy /
// AddPolicy / RemovePolicy / RemoveFilteredPolicy) plus the optional
// persist.BatchAdapter (AddPolicies / RemovePolicies) so SyncedEnforcer
// can batch-write during Bootstrap and avoid one round-trip per row.
//
// All methods are safe to call from multiple goroutines; the
// underlying *gorm.DB connection pool serialises access, and Casbin
// itself wraps Authorize / policy-mutating calls in its own RWMutex
// (SyncedEnforcer).
type gormAdapter struct {
	db *gorm.DB
}

// newGormAdapter constructs the adapter. It runs no DDL: casbin_rule
// creation moved into the authz module's Migrate phase (SPEC §5.3 —
// battery tables follow the framework migrate mode; off means the
// framework touches no schema, this table included). Construction
// against a database without the table succeeds; the first LoadPolicy
// fails instead, which is the fail-closed startup surface.
func newGormAdapter(db *gorm.DB) (persist.Adapter, error) {
	if db == nil {
		return nil, errors.New("authz/casbin: nil *gorm.DB")
	}
	return &gormAdapter{db: db}, nil
}

// LoadPolicy reads every casbin_rule row and feeds them to the
// in-memory Casbin model via persist.LoadPolicyArray.
//
// We deliberately bypass persist.LoadPolicyLine: that helper rebuilds
// the row into a CSV string and re-parses it through csv.Reader, which
// mis-splits any v0..v5 value containing the delimiter (", "), an
// embedded quote, or leading whitespace. A subject like
// "task,delete" inserted via AddPolicy would round-trip as two
// fields under the line path and either corrupt the loaded model or
// fail with an arity mismatch at app startup. Handing the row to
// LoadPolicyArray as a []string keeps every byte intact.
//
// Order("id") makes load order deterministic across SQL drivers —
// without it, Casbin's first-match matcher iteration becomes
// driver-dependent.
func (a *gormAdapter) LoadPolicy(m model.Model) error {
	var rules []CasbinRule
	if err := a.db.Order("id").Find(&rules).Error; err != nil {
		return fmt.Errorf("authz/casbin LoadPolicy: %w", err)
	}
	for _, r := range rules {
		// Empty Ptype is never produced by AddPolicy / AddPolicies (Casbin
		// always passes "p" / "g"), but the column has no NOT NULL —
		// operator SQL or import from another store could leave one. Without
		// this guard, persist.LoadPolicyArray panics with "slice bounds out
		// of range" on `key[:1]`, which bricks app startup on a single bad
		// row. Refuse with a row-id-bearing error instead so the operator
		// can locate and clean the row.
		if r.Ptype == "" {
			return fmt.Errorf("authz/casbin LoadPolicy: row id=%d has empty Ptype; suspect manual SQL or non-Casbin import — clean the row before retrying", r.ID)
		}
		if err := persist.LoadPolicyArray(rowToLoadArray(r), m); err != nil {
			return fmt.Errorf("authz/casbin LoadPolicy id=%d ptype=%s: %w", r.ID, r.Ptype, err)
		}
	}
	return nil
}

// rowToLoadArray builds the [ptype, v0, v1, ...] slice
// persist.LoadPolicyArray expects. Trailing empty Vn are elided so
// the model's policy section matches the shape AddPolicy originally
// inserted — Casbin treats trailing empties as explicit fields, which
// would diverge from what GetPolicy / GetFilteredPolicy reports back
// to Service callers.
func rowToLoadArray(r CasbinRule) []string {
	rule := rowToRule(r)
	out := make([]string, 0, 1+len(rule))
	out = append(out, r.Ptype)
	return append(out, rule...)
}

// SavePolicy is the bulk-replace path Casbin uses when an operator
// calls enforcer.SavePolicy(). chok's typical flow is incremental
// (AddPolicy / RemovePolicy via the Service interface), so this
// method is rarely hit; we keep it correct but optimise the
// incremental path elsewhere.
//
// We delete every existing row in a transaction, then insert the
// freshly-serialised model. Truncate-and-replace mirrors gorm-
// adapter v3's behaviour and keeps SavePolicy semantics consistent
// across adapter swaps.
func (a *gormAdapter) SavePolicy(m model.Model) error {
	var rows []CasbinRule
	for _, sec := range []string{"p", "g"} {
		ast, ok := m[sec]
		if !ok {
			continue
		}
		for ptype, assertion := range ast {
			for _, rule := range assertion.Policy {
				row, err := ruleToRow(ptype, rule)
				if err != nil {
					return fmt.Errorf("SavePolicy: %w", err)
				}
				rows = append(rows, row)
			}
		}
	}
	return a.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Session(&gorm.Session{AllowGlobalUpdate: true}).
			Delete(&CasbinRule{}).Error; err != nil {
			return fmt.Errorf("SavePolicy clear: %w", err)
		}
		if len(rows) == 0 {
			return nil
		}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).
			Create(&rows).Error; err != nil {
			return fmt.Errorf("SavePolicy insert: %w", err)
		}
		return nil
	})
}

// AddPolicy inserts one row. SyncedEnforcer's policy-mutating call
// chain converges here for both p (permission) and g (grouping)
// sections.
//
// The composite unique index on (ptype, v0..v5) plus
// clause.OnConflict{DoNothing: true} make this idempotent at the
// database layer: re-running Bootstrap or two pods racing to insert
// the same tuple converge to a single row instead of growing
// duplicates that LoadPolicy would later have to dedupe.
func (a *gormAdapter) AddPolicy(_, ptype string, rule []string) error {
	row, err := ruleToRow(ptype, rule)
	if err != nil {
		return fmt.Errorf("authz/casbin AddPolicy: %w", err)
	}
	if err := a.db.Clauses(clause.OnConflict{DoNothing: true}).
		Create(&row).Error; err != nil {
		return fmt.Errorf("authz/casbin AddPolicy: %w", err)
	}
	return nil
}

// RemovePolicy deletes rows matching (ptype, v0, v1, ...). Trailing
// rule columns that aren't supplied are not constrained — equivalent
// to gorm-adapter's "exact-match against the supplied prefix"
// semantics.
func (a *gormAdapter) RemovePolicy(_, ptype string, rule []string) error {
	q := a.db.Where("ptype = ?", ptype)
	q = applyValueColumns(q, 0, rule)
	if err := q.Delete(&CasbinRule{}).Error; err != nil {
		return fmt.Errorf("authz/casbin RemovePolicy: %w", err)
	}
	return nil
}

// RemoveFilteredPolicy deletes rows matching ptype + the values
// supplied at fieldIndex onwards. Empty values in fieldValues are
// wildcards (skipped from the WHERE clause), matching Casbin's
// documented semantics.
//
// Safety guard: a filter that produces zero MAPPED non-empty
// constraints reduces to "DELETE every row of this ptype" because
// every column constraint gets skipped. The degenerate cases are
// (a) all-empty fieldValues, (b) fieldIndex past V5 so no value
// lands in a real column (the inner helper breaks immediately), and
// (c) negative fieldIndex (would otherwise panic in cols[idx]).
// Casbin's RBAC API never legitimately calls in this shape — even
// DeleteRolesForUser supplies the user as v0 — so we reject as a
// footgun rather than silently wipe a section. Code that actually
// wants "clear all p-rules" should call SavePolicy with an empty
// model, which is the documented bulk-replace path.
func (a *gormAdapter) RemoveFilteredPolicy(_, ptype string, fieldIndex int, fieldValues ...string) error {
	if !hasAnyMappedConstraint(fieldIndex, fieldValues) {
		return fmt.Errorf("authz/casbin RemoveFilteredPolicy: refusing to delete all rows of ptype %q — no mapped non-empty constraint (all-empty fieldValues, fieldIndex past V5, or negative fieldIndex); use SavePolicy(emptyModel) for a bulk clear", ptype)
	}
	q := a.db.Where("ptype = ?", ptype)
	q = applyValueColumnsFiltered(q, fieldIndex, fieldValues)
	if err := q.Delete(&CasbinRule{}).Error; err != nil {
		return fmt.Errorf("authz/casbin RemoveFilteredPolicy: %w", err)
	}
	return nil
}

// hasAnyMappedConstraint reports whether at least one fieldValues
// entry both (a) maps to a real storage column V0..V5 (i.e.
// fieldIndex+i is in [0, maxRuleColumns)) and (b) is non-empty.
// Filtered-mutation paths use this to refuse calls that would
// degenerate to a ptype-wide delete: a non-empty value alone is not
// enough — fieldIndex=6 with fieldValues=["x"] looks "constrained"
// but applyValueColumnsFiltered skips every column (idx >= 6 → break)
// and the WHERE collapses to ptype=?. Negative fieldIndex is also
// rejected here so the inner helper never indexes cols[-1].
func hasAnyMappedConstraint(fieldIndex int, fieldValues []string) bool {
	if fieldIndex < 0 {
		return false
	}
	for i, v := range fieldValues {
		idx := fieldIndex + i
		if idx >= maxRuleColumns {
			break
		}
		if v != "" {
			return true
		}
	}
	return false
}

// AddPolicies (BatchAdapter) inserts many rows in one transaction.
// Bootstrap-style writes hit this path; without it, seeding 100
// permissions would issue 100 INSERTs and 100 round-trips. Like
// AddPolicy, this uses OnConflict{DoNothing:true} so concurrent
// instances bootstrapping the same admin permissions converge on a
// single row per tuple.
func (a *gormAdapter) AddPolicies(_, ptype string, rules [][]string) error {
	if len(rules) == 0 {
		return nil
	}
	rows := make([]CasbinRule, 0, len(rules))
	for _, r := range rules {
		row, err := ruleToRow(ptype, r)
		if err != nil {
			return fmt.Errorf("authz/casbin AddPolicies: %w", err)
		}
		rows = append(rows, row)
	}
	if err := a.db.Clauses(clause.OnConflict{DoNothing: true}).
		Create(&rows).Error; err != nil {
		return fmt.Errorf("authz/casbin AddPolicies: %w", err)
	}
	return nil
}

// RemovePolicies (BatchAdapter) deletes many rows in one transaction.
// Each rule's values translate to a separate AND-constrained DELETE,
// joined under a single transaction so a partial failure rolls back.
func (a *gormAdapter) RemovePolicies(_, ptype string, rules [][]string) error {
	if len(rules) == 0 {
		return nil
	}
	return a.db.Transaction(func(tx *gorm.DB) error {
		for _, r := range rules {
			q := tx.Where("ptype = ?", ptype)
			q = applyValueColumns(q, 0, r)
			if err := q.Delete(&CasbinRule{}).Error; err != nil {
				return fmt.Errorf("authz/casbin RemovePolicies: %w", err)
			}
		}
		return nil
	})
}

// UpdatePolicy (UpdatableAdapter) atomically replaces oldRule with
// newRule for a given (sec, ptype). Casbin v3's enforcer.UpdatePolicy
// path does a hard `e.adapter.(persist.UpdatableAdapter)` assertion
// (internal_api.go:171) and panics when the adapter doesn't satisfy
// it — so this isn't optional even though chok's Service interface
// doesn't yet expose UpdatePolicy. Implementing it keeps the adapter
// contract complete and matches gorm-adapter v3's surface.
//
// Exact-rule semantics: applyExactRule pins every storage column
// V0..V5 (with "" for columns past the rule length). A prefix-only
// WHERE — what the round-2 implementation did via applyValueColumns
// — could match more rows than the rule represents, e.g. an
// oldRule=["alice"] would have deleted every v0=alice row regardless
// of v1..v5. Casbin's model.UpdatePolicy then performs an in-memory
// exact-rule update and returns false on no match, leaving the DB
// missing rules the model still believes are there: a silent
// store/model divergence. RowsAffected==0 after the targeted Delete
// means the oldRule wasn't present, so we abort before inserting
// (no insert without a corresponding delete).
//
// Atomic by transaction: a partial failure (delete miss, insert
// conflict against an unrelated row) rolls back. Insert deliberately
// does NOT use OnConflict{DoNothing:true} — Update means replace,
// not merge; a unique-index conflict against an unrelated tuple
// must surface as an error so the caller can react.
func (a *gormAdapter) UpdatePolicy(_, ptype string, oldRule, newRule []string) error {
	if len(oldRule) > maxRuleColumns {
		return fmt.Errorf("authz/casbin UpdatePolicy: oldRule has %d fields, max supported is %d", len(oldRule), maxRuleColumns)
	}
	newRow, err := ruleToRow(ptype, newRule)
	if err != nil {
		return fmt.Errorf("authz/casbin UpdatePolicy: %w", err)
	}
	return a.db.Transaction(func(tx *gorm.DB) error {
		q := tx.Where("ptype = ?", ptype)
		q = applyExactRule(q, oldRule)
		res := q.Delete(&CasbinRule{})
		if err := res.Error; err != nil {
			return fmt.Errorf("authz/casbin UpdatePolicy delete: %w", err)
		}
		if res.RowsAffected == 0 {
			return fmt.Errorf("authz/casbin UpdatePolicy: oldRule %v not found for ptype %q (would diverge from model.UpdatePolicy which also no-ops on no-match)", oldRule, ptype)
		}
		if err := tx.Create(&newRow).Error; err != nil {
			return fmt.Errorf("authz/casbin UpdatePolicy insert: %w", err)
		}
		return nil
	})
}

// UpdatePolicies (UpdatableAdapter) replaces N old rules with N new
// rules in a single transaction. Casbin guarantees len(oldRules) ==
// len(newRules) before calling (internal_api.go:202-204) — we
// re-check defensively so a future Casbin upgrade that drops the
// pre-check still fails fast here.
//
// Same exact-rule + RowsAffected==0 contract as UpdatePolicy: any
// oldRule that doesn't match an existing row aborts the whole
// transaction, so a partial-miss batch leaves the store unchanged.
// The alternative — best-effort delete-what-you-can — would diverge
// from model.UpdatePolicies which only succeeds when every rule was
// found.
func (a *gormAdapter) UpdatePolicies(_, ptype string, oldRules, newRules [][]string) error {
	if len(oldRules) != len(newRules) {
		return fmt.Errorf("authz/casbin UpdatePolicies: oldRules length %d != newRules length %d", len(oldRules), len(newRules))
	}
	if len(oldRules) == 0 {
		return nil
	}
	for i, r := range oldRules {
		if len(r) > maxRuleColumns {
			return fmt.Errorf("authz/casbin UpdatePolicies: oldRules[%d] has %d fields, max supported is %d", i, len(r), maxRuleColumns)
		}
	}
	newRows := make([]CasbinRule, 0, len(newRules))
	for _, r := range newRules {
		row, err := ruleToRow(ptype, r)
		if err != nil {
			return fmt.Errorf("authz/casbin UpdatePolicies: %w", err)
		}
		newRows = append(newRows, row)
	}
	return a.db.Transaction(func(tx *gorm.DB) error {
		for i, r := range oldRules {
			q := tx.Where("ptype = ?", ptype)
			q = applyExactRule(q, r)
			res := q.Delete(&CasbinRule{})
			if err := res.Error; err != nil {
				return fmt.Errorf("authz/casbin UpdatePolicies delete[%d]: %w", i, err)
			}
			if res.RowsAffected == 0 {
				return fmt.Errorf("authz/casbin UpdatePolicies: oldRules[%d] %v not found for ptype %q (rolling back batch to keep store consistent with model)", i, r, ptype)
			}
		}
		if err := tx.Create(&newRows).Error; err != nil {
			return fmt.Errorf("authz/casbin UpdatePolicies insert: %w", err)
		}
		return nil
	})
}

// UpdateFilteredPolicies (UpdatableAdapter) deletes rows matching the
// filter and inserts newRules. Returns the deleted rows so Casbin's
// in-memory model can surface them to the watcher path. Two guards:
//
//  1. hasAnyMappedConstraint — refuses calls that would degenerate to
//     "replace every row of this ptype with newRules" (all-empty
//     fieldValues, fieldIndex past V5, negative fieldIndex).
//
//  2. zero-hit + non-empty newRules — refuses calls where the filter
//     matched no existing rules but the caller still wants to insert
//     N new ones. Casbin v3.10.0's enforcer.updateFilteredPolicies-
//     WithoutNotify (internal_api.go:317-374) treats an empty
//     oldRules return as "no rule changed" and skips watcher
//     notification, but the upper-layer model.AddPolicies(newRules)
//     still runs. If we ALSO inserted newRules here, the local
//     store + model would hold them while peer instances never see
//     the watcher event — silent multi-instance divergence. Forcing
//     the caller to use AddPolicies for pure inserts keeps that path
//     clean.
func (a *gormAdapter) UpdateFilteredPolicies(_, ptype string, newRules [][]string, fieldIndex int, fieldValues ...string) ([][]string, error) {
	if !hasAnyMappedConstraint(fieldIndex, fieldValues) {
		return nil, fmt.Errorf("authz/casbin UpdateFilteredPolicies: refusing to replace all rows of ptype %q — no mapped non-empty constraint (all-empty fieldValues, fieldIndex past V5, or negative fieldIndex); supply at least one constraint or use SavePolicy for a bulk replace", ptype)
	}
	newRows := make([]CasbinRule, 0, len(newRules))
	for _, r := range newRules {
		row, err := ruleToRow(ptype, r)
		if err != nil {
			return nil, fmt.Errorf("authz/casbin UpdateFilteredPolicies: %w", err)
		}
		newRows = append(newRows, row)
	}

	var oldRows []CasbinRule
	err := a.db.Transaction(func(tx *gorm.DB) error {
		q := tx.Where("ptype = ?", ptype)
		q = applyValueColumnsFiltered(q, fieldIndex, fieldValues)
		if err := q.Find(&oldRows).Error; err != nil {
			return fmt.Errorf("authz/casbin UpdateFilteredPolicies select: %w", err)
		}
		if len(oldRows) == 0 && len(newRows) > 0 {
			return fmt.Errorf("authz/casbin UpdateFilteredPolicies: filter matched no rules of ptype %q but %d new rules supplied; use AddPolicies for a pure insert (avoids store/model divergence under Casbin's empty-oldRules quirk)", ptype, len(newRows))
		}
		// Re-build the WHERE chain on tx for the Delete: GORM consumes
		// the chain on the Find above, so we need a fresh Where set.
		dq := tx.Where("ptype = ?", ptype)
		dq = applyValueColumnsFiltered(dq, fieldIndex, fieldValues)
		if err := dq.Delete(&CasbinRule{}).Error; err != nil {
			return fmt.Errorf("authz/casbin UpdateFilteredPolicies delete: %w", err)
		}
		if len(newRows) == 0 {
			return nil
		}
		if err := tx.Create(&newRows).Error; err != nil {
			return fmt.Errorf("authz/casbin UpdateFilteredPolicies insert: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make([][]string, 0, len(oldRows))
	for _, r := range oldRows {
		out = append(out, rowToRule(r))
	}
	return out, nil
}

// rowToRule projects a stored row back into a Casbin rule slice,
// trimming trailing empties so the result matches what AddPolicy
// originally received. Used by UpdateFilteredPolicies' return path
// so callers (and watchers) see the same shape they wrote.
func rowToRule(r CasbinRule) []string {
	values := []string{r.V0, r.V1, r.V2, r.V3, r.V4, r.V5}
	end := 0
	for i := len(values) - 1; i >= 0; i-- {
		if values[i] != "" {
			end = i + 1
			break
		}
	}
	if end == 0 {
		return nil
	}
	out := make([]string, end)
	copy(out, values[:end])
	return out
}

// ruleToRow projects a Casbin rule slice into the storage struct.
// Returns an error when the rule has more fields than the adapter can
// store (V0..V5 = 6 values). Custom Options.Model with policy width >
// 6 is unsupported — silently truncating would corrupt SavePolicy
// round-trip.
func ruleToRow(ptype string, rule []string) (CasbinRule, error) {
	if len(rule) > maxRuleColumns {
		return CasbinRule{}, fmt.Errorf(
			"policy rule has %d fields, max supported is %d (custom Casbin model with more than V0..V5 not supported by chok adapter)",
			len(rule), maxRuleColumns)
	}
	r := CasbinRule{Ptype: ptype}
	cols := []*string{&r.V0, &r.V1, &r.V2, &r.V3, &r.V4, &r.V5}
	for i, v := range rule {
		*cols[i] = v
	}
	return r, nil
}

// applyValueColumns appends WHERE clauses for every supplied rule
// value at columns v{startIdx}, v{startIdx+1}, .... Used by
// RemovePolicy where every value is a hard match (no wildcards).
//
// NOTE: this is a PREFIX match — columns past len(rule) are left
// unconstrained. UpdatePolicy / UpdatePolicies must NOT use this
// helper; they need applyExactRule to honour Casbin's exact-rule
// update contract. RemovePolicy keeps prefix semantics because
// Casbin passes the full-arity rule there and trailing columns are
// "" by convention, so prefix and exact behave identically for
// well-formed input.
func applyValueColumns(q *gorm.DB, startIdx int, rule []string) *gorm.DB {
	cols := []string{"v0", "v1", "v2", "v3", "v4", "v5"}
	for i, v := range rule {
		idx := startIdx + i
		if idx >= len(cols) {
			break
		}
		q = q.Where(cols[idx]+" = ?", v)
	}
	return q
}

// applyExactRule appends WHERE clauses pinning every storage column
// V0..V5 to rule's value (or "" when rule is shorter than 6). Used
// by Update* paths because Casbin's UpdatePolicy contract is
// "replace this exact rule": a prefix-only WHERE would silently
// delete rows whose trailing v_n are non-empty but front prefix
// matches, leaving model.UpdatePolicy (which only no-ops on no
// match) inconsistent with the store. Callers must validate
// len(rule) <= maxRuleColumns before invoking — applyExactRule does
// not enforce that and would silently truncate to the first six.
func applyExactRule(q *gorm.DB, rule []string) *gorm.DB {
	cols := []string{"v0", "v1", "v2", "v3", "v4", "v5"}
	for i, c := range cols {
		if i < len(rule) {
			q = q.Where(c+" = ?", rule[i])
		} else {
			q = q.Where(c+" = ?", "")
		}
	}
	return q
}

// applyValueColumnsFiltered is RemoveFilteredPolicy's helper: empty
// values in fieldValues are treated as wildcards (skipped) rather
// than literal "" matches.
func applyValueColumnsFiltered(q *gorm.DB, fieldIndex int, fieldValues []string) *gorm.DB {
	cols := []string{"v0", "v1", "v2", "v3", "v4", "v5"}
	for i, v := range fieldValues {
		idx := fieldIndex + i
		if idx >= len(cols) {
			break
		}
		if v == "" {
			continue
		}
		q = q.Where(cols[idx]+" = ?", v)
	}
	return q
}

// Compile-time interface assertions. SyncedEnforcer requires
// persist.Adapter; persist.BatchAdapter is optional but lets
// AddPolicies / RemovePolicies bypass per-row round-trips.
//
// persist.UpdatableAdapter is also asserted because Casbin v3's
// enforcer.UpdatePolicy / UpdatePolicies / UpdateFilteredPolicies
// path does a hard `e.adapter.(persist.UpdatableAdapter)` assertion
// (internal_api.go:171, :211, :328) and panics when the adapter
// doesn't satisfy it. chok's Service interface doesn't currently
// expose Update*, but a future Service extension or any caller that
// reaches the underlying enforcer must not crash on it.
var (
	_ persist.Adapter          = (*gormAdapter)(nil)
	_ persist.BatchAdapter     = (*gormAdapter)(nil)
	_ persist.UpdatableAdapter = (*gormAdapter)(nil)
)
