package identity

import (
	"context"
	"errors"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"gorm.io/gorm"

	"github.com/zynthara/chok/v3"
	"github.com/zynthara/chok/v3/store"
	"github.com/zynthara/chok/v3/store/where"
)

// UserService is the administration of accounts — what an operator does
// to a user, as opposed to what users do through the routes. A missing
// user is a 404 error; every change that affects the user's tokens
// (password, roles, active flag) revokes them, so the next request logs
// in again and picks the change up.
type UserService struct{ idn *Identity }

// bumpVersion moves password_version on, atomically in the database, so
// concurrent revocations each count.
var bumpVersion = gorm.Expr("password_version + 1")

// The limits of a role: Roles is stored comma-separated in 500 bytes.
const (
	roleMaxLen    = 32
	rolesMaxCSV   = 500
	roleForbidden = ","
)

// adminRole is the built-in administrator role: it reads the audit log,
// identity/rbac lets it do everything, and auth.admins grants it.
const adminRole = "admin"

// The user list's pages: ListFromQuery.
const (
	userPageSize = 20
	userMaxPage  = 100
)

// userFields are the fields ListFromQuery filters and orders on, by
// their names in the query string.
var userFields = map[string]string{
	"active": "active", "created_at": "created_at", "email": "email", "email_verified": "email_verified", "name": "name",
}

var (
	errNoUserID = chok.ErrBadRequest.Msgf("a user id is required")
	errNoEmail  = chok.ErrBadRequest.Msgf("an email is required")
)

func notFoundUser(err error) error {
	if errors.Is(err, chok.ErrNotFound) {
		return chok.ErrNotFound.Msgf("user not found")
	}
	return err
}

// Create makes an active account with a password, as register does: for
// a deployment that turns registration off and provisions accounts.
func (s *UserService) Create(ctx context.Context, email, password, name string) (*User, error) {
	return s.idn.createUser(ctx, email, password, name)
}

// Get returns the user with id (usr_…).
func (s *UserService) Get(ctx context.Context, id string) (*User, error) {
	if id == "" {
		return nil, errNoUserID
	}
	u, err := s.idn.users.Get(ctx, store.RID(id))
	if err != nil {
		return nil, notFoundUser(err)
	}
	return u, nil
}

// GetByEmail returns the user with the email address, compared as
// identity stores addresses: trimmed and in lower case.
func (s *UserService) GetByEmail(ctx context.Context, email string) (*User, error) {
	email = normalizeEmail(email)
	if email == "" {
		return nil, errNoEmail
	}
	u, err := s.idn.users.Get(ctx, store.Where(where.Eq("email", email)))
	if err != nil {
		return nil, notFoundUser(err)
	}
	return u, nil
}

// QueryFields lists the fields ListFromQuery filters and orders on.
func (s *UserService) QueryFields() []string {
	return slices.Sorted(maps.Keys(userFields))
}

// ListFromQuery returns a page of every account, for an administration
// page: q holds page and size (default 20, at most 100), cursor,
// order=field:asc|desc and equality filters on the fields QueryFields
// lists — email compared as identity stores it, email_verified and
// active as true or false. An unknown or malformed parameter is a 400.
// The items are User's JSON, which leaves out the roles and the active
// flag.
//
// It lets the UserService serve a chok.List route, and that route lists
// every user: mount it only behind an administrator's authorization —
// rbac's Require, or a middleware of the application's — never behind
// Authn alone:
//
//	admin := app.Group("/admin", auth.Authn(), rb.Require("user", "read"))
//	chok.List(admin, "/users", auth.Users())
func (s *UserService) ListFromQuery(ctx context.Context, q url.Values) (*chok.Page[User], error) {
	q = maps.Clone(q)
	var flags []where.Option
	for _, key := range []string{"active", "email_verified"} {
		vals, ok := q[key]
		if !ok {
			continue
		}
		delete(q, key)
		var in []any
		for _, v := range vals {
			if v == "" {
				continue
			}
			b, err := strconv.ParseBool(v)
			if err != nil {
				return nil, chok.ErrBadRequest.Msgf("%s is true or false, got %q", key, v).With("field", key)
			}
			in = append(in, b)
		}
		switch len(in) {
		case 0:
		case 1:
			flags = append(flags, where.Eq(key, in[0]))
		default:
			flags = append(flags, where.In(key, in...))
		}
	}
	if emails, ok := q["email"]; ok {
		normalized := make([]string, len(emails))
		for i, e := range emails {
			normalized[i] = normalizeEmail(e)
		}
		q["email"] = normalized
	}
	opts, err := where.FromQuery(q, userFields, true)
	if err != nil {
		return nil, chok.ErrBadRequest.Msgf("%s", strings.TrimPrefix(err.Error(), "where: ")).Wrap(err)
	}
	return s.idn.users.List(ctx, slices.Concat(opts, flags, []where.Option{where.Count()})...)
}

// SetActive enables or disables the account. Either way its tokens stop
// working: a disabled user is out at once, a re-enabled one logs in anew.
func (s *UserService) SetActive(ctx context.Context, id string, active bool) error {
	return s.update(ctx, id, map[string]any{"active": active, "password_version": bumpVersion})
}

// SetRoles replaces the user's roles — nil or empty clears them — and
// revokes the user's tokens, so the next login carries the new roles. A
// role is at most 32 characters and has no comma; together they fit in
// 500 bytes.
func (s *UserService) SetRoles(ctx context.Context, id string, roles []string) error {
	if err := checkRoles(roles); err != nil {
		return err
	}
	return s.update(ctx, id, map[string]any{"roles": strings.Join(roles, ","), "password_version": bumpVersion})
}

// ResetPassword sets a new password, held to the register route's rules,
// and revokes the user's tokens.
func (s *UserService) ResetPassword(ctx context.Context, id, password string) error {
	if id == "" {
		return errNoUserID
	}
	if len(password) < 8 || len(password) > 72 {
		return chok.ErrValidation.Msgf("password must be 8 to 72 bytes").With("fields", map[string]string{"password": "length"})
	}
	return notFoundUser(s.idn.setPassword(ctx, id, "password", password))
}

// RevokeTokens makes every token issued to the user stop working, and
// changes nothing else: for a leaked token, or a log-out everywhere.
func (s *UserService) RevokeTokens(ctx context.Context, id string) error {
	return s.update(ctx, id, map[string]any{"password_version": bumpVersion})
}

// MarkEmailVerified records that the user proved the email address, after
// the application's own verification (a link, a code). Tokens stay
// valid; calling it again is a no-op. As with a mailed reset token or
// code, the first verification unlinks the providers linked before it:
// identity links none to an unverified account, but data from v2 may.
func (s *UserService) MarkEmailVerified(ctx context.Context, id string) error {
	u, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	return notFoundUser(s.idn.verifyEmail(ctx, u, map[string]any{}))
}

func (s *UserService) update(ctx context.Context, id string, set map[string]any) error {
	if id == "" {
		return errNoUserID
	}
	return notFoundUser(s.idn.users.Update(ctx, store.RID(id), store.Set(set)))
}

func checkRoles(roles []string) error {
	total := 0
	for i, r := range roles {
		switch {
		case r == "":
			return chok.ErrBadRequest.Msgf("a role cannot be empty")
		case strings.ContainsAny(r, roleForbidden):
			return chok.ErrBadRequest.Msgf("role %q contains a comma", r)
		case len(r) > roleMaxLen:
			return chok.ErrBadRequest.Msgf("role %q is longer than %d characters", r, roleMaxLen)
		}
		if i > 0 {
			total++
		}
		total += len(r)
	}
	if total > rolesMaxCSV {
		return chok.ErrBadRequest.Msgf("the roles take %d bytes together; the limit is %d", total, rolesMaxCSV)
	}
	return nil
}
