package auth

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"github.com/scalecraft/darkplum/api/internal/activity"
)

// CreateLocalAdmin provisions a local password admin directly. Unlike Setup it
// has no "first user only" guard — a break-glass admin may be created when one
// already exists — and it never initializes OIDC, so it works even when SSO
// config is broken and the server itself won't boot. Hashing reuses the same
// pepper+bcrypt scheme as the login path so the credential verifies normally.
//
// Pepper compatibility: this hashes with whatever PASSWORD_PEPPER is in the
// process env (loadPepper). When run from the darkplum CLI's bootstrap Job, that
// env comes via envFrom on the darkplum-auth secret. If you ever set
// PASSWORD_PEPPER on the api deployment, it MUST also live in darkplum-auth, or
// the bootstrap admin's hash will not verify at login. See TestPepperHashRoundTrips.
func CreateLocalAdmin(ctx context.Context, db *pgxpool.Pool, rec *activity.Recorder, email, name, password string) (string, error) {
	if email == "" || name == "" {
		return "", errors.New("email and name are required")
	}
	if len(password) < 8 {
		return "", errors.New("password must be at least 8 characters")
	}

	hash, err := bcrypt.GenerateFromPassword(PepperPassword([]byte(password), loadPepper()), bcryptCost)
	if err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}

	var userID string
	err = db.QueryRow(ctx, `
		INSERT INTO users (email, name, role, password_hash, auth_provider)
		VALUES ($1, $2, 'admin', $3, 'local')
		RETURNING id
	`, email, name, string(hash)).Scan(&userID)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return "", fmt.Errorf("a user with email %q already exists", email)
		}
		return "", fmt.Errorf("insert admin: %w", err)
	}

	if rec != nil {
		rec.Record(ctx, activity.Event{
			Type:       activity.UserCreated,
			TargetType: activity.StrPtr("user"),
			TargetID:   activity.StrPtr(userID),
			Detail:     map[string]any{"email": email, "name": name, "via": "darkplum admin create"},
		})
	}

	return userID, nil
}
