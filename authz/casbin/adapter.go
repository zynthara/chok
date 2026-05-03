package casbin

import (
	"context"
	"errors"
	"fmt"

	"github.com/casbin/casbin/v3/model"
	"github.com/casbin/casbin/v3/persist"
	"gorm.io/gorm"
)

// CasbinRule is the chok-shipped storage row for Casbin policies.
// Wire-compatible with gorm-adapter v3's table layout (table name +
// columns + sizes), so a deployment that has already run with
// gorm-adapter can switch to the chok adapter without migration.
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
	Ptype string `gorm:"size:100;index:idx_casbin_rule_ptype"`
	V0    string `gorm:"size:100"`
	V1    string `gorm:"size:100"`
	V2    string `gorm:"size:100"`
	V3    string `gorm:"size:100"`
	V4    string `gorm:"size:100"`
	V5    string `gorm:"size:100"`
}

// TableName pins the storage name to "casbin_rule" so existing data
// from gorm-adapter v3 deployments stays accessible.
func (CasbinRule) TableName() string { return "casbin_rule" }

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

// newGormAdapter constructs the adapter and runs AutoMigrate so the
// casbin_rule table exists. AutoMigrate is idempotent: subsequent
// startups see the table and no-op.
func newGormAdapter(db *gorm.DB) (persist.Adapter, error) {
	if db == nil {
		return nil, errors.New("authz/casbin: nil *gorm.DB")
	}
	if err := db.AutoMigrate(&CasbinRule{}); err != nil {
		return nil, fmt.Errorf("authz/casbin: AutoMigrate casbin_rule: %w", err)
	}
	return &gormAdapter{db: db}, nil
}

// LoadPolicy reads every casbin_rule row and feeds them to the
// in-memory Casbin model via persist.LoadPolicyLine. The line format
// is "ptype, v0, v1, ..." stopping at the first empty Vn — Casbin
// trims trailing empty fields automatically.
func (a *gormAdapter) LoadPolicy(m model.Model) error {
	var rules []CasbinRule
	if err := a.db.Find(&rules).Error; err != nil {
		return fmt.Errorf("authz/casbin LoadPolicy: %w", err)
	}
	for _, r := range rules {
		if err := persist.LoadPolicyLine(formatPolicyLine(r), m); err != nil {
			return fmt.Errorf("authz/casbin LoadPolicy parse %q: %w", formatPolicyLine(r), err)
		}
	}
	return nil
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
	return a.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Session(&gorm.Session{AllowGlobalUpdate: true}).
			Delete(&CasbinRule{}).Error; err != nil {
			return fmt.Errorf("SavePolicy clear: %w", err)
		}
		var rows []CasbinRule
		for _, sec := range []string{"p", "g"} {
			ast, ok := m[sec]
			if !ok {
				continue
			}
			for ptype, assertion := range ast {
				for _, rule := range assertion.Policy {
					rows = append(rows, ruleToRow(ptype, rule))
				}
			}
		}
		if len(rows) == 0 {
			return nil
		}
		if err := tx.Create(&rows).Error; err != nil {
			return fmt.Errorf("SavePolicy insert: %w", err)
		}
		return nil
	})
}

// AddPolicy inserts one row. SyncedEnforcer's policy-mutating call
// chain converges here for both p (permission) and g (grouping) sections.
//
// Returns nil even when the row already exists; gorm's Create with a
// duplicate primary key would normally error, but we don't have a
// unique index on (ptype, v0..v5) so this is plain INSERT semantics
// and duplicates produce two rows. Casbin upper layer dedupes during
// LoadPolicy via the in-memory model, but operators running raw
// AddPolicy calls bypass the model — same caveat as gorm-adapter v3.
func (a *gormAdapter) AddPolicy(_, ptype string, rule []string) error {
	row := ruleToRow(ptype, rule)
	if err := a.db.Create(&row).Error; err != nil {
		return fmt.Errorf("authz/casbin AddPolicy: %w", err)
	}
	return nil
}

// RemovePolicy deletes rows matching (ptype, v0, v1, ...). Trailing
// rule columns that aren't supplied are not constrained — equivalent
// to gorm-adapter's "exact-match against the supplied prefix" semantics.
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
func (a *gormAdapter) RemoveFilteredPolicy(_, ptype string, fieldIndex int, fieldValues ...string) error {
	q := a.db.Where("ptype = ?", ptype)
	q = applyValueColumnsFiltered(q, fieldIndex, fieldValues)
	if err := q.Delete(&CasbinRule{}).Error; err != nil {
		return fmt.Errorf("authz/casbin RemoveFilteredPolicy: %w", err)
	}
	return nil
}

// AddPolicies (BatchAdapter) inserts many rows in one transaction.
// Bootstrap-style writes hit this path; without it, seeding 100
// permissions would issue 100 INSERTs and 100 round-trips.
func (a *gormAdapter) AddPolicies(_, ptype string, rules [][]string) error {
	if len(rules) == 0 {
		return nil
	}
	rows := make([]CasbinRule, 0, len(rules))
	for _, r := range rules {
		rows = append(rows, ruleToRow(ptype, r))
	}
	if err := a.db.Create(&rows).Error; err != nil {
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

// formatPolicyLine builds the comma-separated string Casbin's
// LoadPolicyLine expects: "ptype, v0, v1, ..." trimmed at the first
// empty trailing Vn.
func formatPolicyLine(r CasbinRule) string {
	values := []string{r.V0, r.V1, r.V2, r.V3, r.V4, r.V5}
	// Walk forward and stop building once we hit the first empty
	// trailing value. Empty values inside the populated prefix are
	// preserved (Casbin treats them as explicit empty fields).
	end := len(values)
	for i := len(values) - 1; i >= 0; i-- {
		if values[i] != "" {
			end = i + 1
			break
		}
		end = i
	}
	out := r.Ptype
	for i := 0; i < end; i++ {
		out += ", " + values[i]
	}
	return out
}

// ruleToRow projects a Casbin rule slice into the storage struct.
func ruleToRow(ptype string, rule []string) CasbinRule {
	r := CasbinRule{Ptype: ptype}
	cols := []*string{&r.V0, &r.V1, &r.V2, &r.V3, &r.V4, &r.V5}
	for i, v := range rule {
		if i >= len(cols) {
			break
		}
		*cols[i] = v
	}
	return r
}

// applyValueColumns appends WHERE clauses for every supplied rule
// value at columns v{startIdx}, v{startIdx+1}, .... Used by
// RemovePolicy where every value is a hard match (no wildcards).
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

// _ keeps context.Background reachable for tests that use it without
// importing context themselves through the adapter.
var _ = context.Background

// Compile-time interface assertions. SyncedEnforcer requires
// persist.Adapter; persist.BatchAdapter is optional but lets
// AddPolicies / RemovePolicies bypass per-row round-trips.
var (
	_ persist.Adapter      = (*gormAdapter)(nil)
	_ persist.BatchAdapter = (*gormAdapter)(nil)
)
