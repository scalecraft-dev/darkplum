package auth

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// userCacheTTL bounds how long a demotion/removal made outside this process
// (or via a path that forgot to invalidate) can go unnoticed. Handlers in this
// service call InvalidateUser so their own changes take effect immediately.
const userCacheTTL = 30 * time.Second

// errUserGone means the session's user row no longer exists. The session must
// be discarded (cookies cleared), not merely rejected.
var errUserGone = errors.New("session user no longer exists")

type cachedUser struct {
	email, name, role string
	expires           time.Time
}

var userCache sync.Map // user id (string) -> cachedUser

// InvalidateUser drops any cached copy of the user. Call after a role change,
// deletion, or any other user mutation so the next request re-reads the row.
func InvalidateUser(id string) { userCache.Delete(id) }

// lookupUser returns the current DB view of the user: one primary-key query,
// cached for userCacheTTL. A missing user is never cached.
func lookupUser(ctx context.Context, db *pgxpool.Pool, id string) (cachedUser, error) {
	if v, ok := userCache.Load(id); ok {
		if u := v.(cachedUser); time.Now().Before(u.expires) {
			return u, nil
		}
		userCache.Delete(id)
	}
	var u cachedUser
	err := db.QueryRow(ctx, `SELECT email, name, role FROM users WHERE id = $1`, id).
		Scan(&u.email, &u.name, &u.role)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			userCache.Delete(id)
			return cachedUser{}, errUserGone
		}
		return cachedUser{}, err
	}
	u.expires = time.Now().Add(userCacheTTL)
	userCache.Store(id, u)
	return u, nil
}

// reconcileClaims makes the DB authoritative: the user must exist and the
// role (and identity fields) come from the row, not the JWT.
func reconcileClaims(ctx context.Context, db *pgxpool.Pool, claims *SessionClaims) (*SessionClaims, error) {
	u, err := lookupUser(ctx, db, claims.UserID)
	if err != nil {
		return nil, err
	}
	out := *claims
	out.Email, out.Name, out.Role = u.email, u.name, u.role
	return &out, nil
}
