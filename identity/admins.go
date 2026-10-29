package identity

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/zynthara/chok/v3"
	"github.com/zynthara/chok/v3/store"
	"github.com/zynthara/chok/v3/store/where"
)

// grantTries bounds the version conflicts grantAdmin rides out before it
// leaves the grant to the user's next request.
const grantTries = 3

// grantAdmin gives u the admin role when auth.admins lists its address,
// the address is verified and u does not hold the role yet. The write is
// conditional on the row's version: of concurrent requests one writes
// and the others, reading the row again, find the role there. It leaves
// password_version alone, so u's tokens stay valid. The grant and its
// audit record commit together: a grant the log cannot record does not
// happen. A failure is a warning: the request goes on without the role,
// and the next one tries again. u is updated in place.
func (idn *Identity) grantAdmin(ctx context.Context, u *User) {
	if len(idn.admins) == 0 {
		return
	}
	logger := chok.LoggerFrom(ctx)
	for try := 1; ; try++ {
		before := u.RoleList()
		if !u.EmailVerified || !idn.admins[u.Email] || slices.Contains(before, adminRole) {
			return
		}
		if try > grantTries {
			logger.WarnContext(ctx, "identity: auth.admins left the grant to the next request: the user kept changing", "user_id", u.RID)
			return
		}
		after := append(slices.Clone(before), adminRole)
		if err := checkRoles(after); err != nil {
			logger.WarnContext(ctx, "identity: auth.admins lists the user, but the admin role does not fit", "user_id", u.RID, "error", err)
			return
		}
		roles := strings.Join(after, ",")
		err := idn.d.RunInTx(ctx, func(ctx context.Context) error {
			if err := idn.users.Update(ctx, store.RID(u.RID), store.Set(map[string]any{"roles": roles}), where.Version(u.Version)); err != nil {
				return err
			}
			return idn.audit.LogSync(ctx, Event{
				Action: "user.role.grant", Resource: "user", ResourceID: u.RID, ActorType: ActorSystem,
				Before: map[string]any{"roles": nonNilRoles(before)}, After: map[string]any{"roles": after},
				Reason: "auth.admins lists the user's verified email",
			})
		})
		if err == nil {
			u.Roles, u.Version = roles, u.Version+1
			logger.InfoContext(ctx, "identity: auth.admins granted the admin role", "user_id", u.RID)
			return
		}
		if !errors.As(err, new(*store.VersionConflictError)) {
			logger.WarnContext(ctx, "identity: auth.admins could not grant the admin role", "user_id", u.RID, "error", err)
			return
		}
		fresh, err := idn.users.Get(ctx, store.RID(u.RID))
		if err != nil {
			logger.WarnContext(ctx, "identity: auth.admins could not read the user again", "user_id", u.RID, "error", err)
			return
		}
		*u = *fresh
	}
}

// nonNilRoles is roles, or an empty list for none: the audit record
// shows [] rather than null.
func nonNilRoles(roles []string) []string {
	if roles == nil {
		return []string{}
	}
	return roles
}
