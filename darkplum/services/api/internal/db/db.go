package db

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"log/slog"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// NewPool creates a pgx connection pool from the given DATABASE_URL.
func NewPool(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database URL: %w", err)
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	return pool, nil
}

// RunMigrations applies any unapplied SQL migrations from the embedded
// migrations directory. Migrations run in filename order (001_, 002_, ...).
// schema_migrations is created first if it does not exist so the runner is
// self-bootstrapping.
func RunMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	// Bootstrap the tracking table before reading it.
	_, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)
	`)
	if err != nil {
		return fmt.Errorf("bootstrap schema_migrations: %w", err)
	}

	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("read embedded migrations: %w", err)
	}

	// Sort by filename to guarantee ascending order.
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}

		version := entry.Name()

		var applied bool
		err := pool.QueryRow(ctx,
			"SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version = $1)",
			version,
		).Scan(&applied)
		if err != nil {
			return fmt.Errorf("check migration %s: %w", version, err)
		}
		if applied {
			continue
		}

		data, err := migrationsFS.ReadFile("migrations/" + version)
		if err != nil {
			return fmt.Errorf("read migration file %s: %w", version, err)
		}

		slog.InfoContext(ctx, "applying migration", "version", version)

		// Each migration runs in its own transaction so a failure is isolated.
		tx, err := pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin transaction for %s: %w", version, err)
		}

		if _, err := tx.Exec(ctx, string(data)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("exec migration %s: %w", version, err)
		}

		if _, err := tx.Exec(ctx,
			"INSERT INTO schema_migrations (version) VALUES ($1)", version,
		); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("record migration %s: %w", version, err)
		}

		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit migration %s: %w", version, err)
		}

		slog.InfoContext(ctx, "migration applied", "version", version)
	}

	return nil
}

// EnsurePartitions creates monthly partitions for inference_events for the
// current month and the next 3 months. It uses CREATE TABLE IF NOT EXISTS so
// it is safe to call on every startup — Postgres no-ops if the partition
// already exists. Any unexpected error (not the expected IF NOT EXISTS no-op)
// is returned so the caller can decide whether to abort startup.
func EnsurePartitions(ctx context.Context, pool *pgxpool.Pool) error {
	now := time.Now().UTC()
	for i := 0; i < 4; i++ {
		month := time.Date(now.Year(), now.Month()+time.Month(i), 1, 0, 0, 0, 0, time.UTC)
		next := month.AddDate(0, 1, 0)
		name := fmt.Sprintf("inference_events_%d_%02d", month.Year(), int(month.Month()))
		ddl := fmt.Sprintf(
			`CREATE TABLE IF NOT EXISTS %s PARTITION OF inference_events FOR VALUES FROM ('%s') TO ('%s')`,
			name,
			month.Format("2006-01-02"),
			next.Format("2006-01-02"),
		)
		if _, err := pool.Exec(ctx, ddl); err != nil {
			slog.ErrorContext(ctx, "ensure partition failed", "name", name, "err", err)
			return fmt.Errorf("ensure partition %s: %w", name, err)
		}
		slog.InfoContext(ctx, "partition ensured", "name", name)
	}
	return nil
}

// Service-key secret source labels, for logging only — the caller must never
// log the secret bytes or the derived key alongside these.
const (
	ServiceKeySecretSourceDedicated = "SERVICE_KEY_SECRET"
	ServiceKeySecretSourceFallback  = "SESSION_SIGNING_KEY (fallback)"
)

// SelectServiceKeySecret decides which secret drives deriveServiceKey.
//
// Rotating the gateway's service credential should not force logging out
// every user, and the blast radius of the session-signing secret (which
// authenticates every human session) should not extend to a standing
// gateway credential used by an entirely different trust boundary. Setting
// SERVICE_KEY_SECRET decouples the two so each can be rotated independently.
// When it's unset, we fall back to sessionSigningKey — today's behavior —
// so existing deployments keep working without a newly-required variable.
//
// It is a pure function so the selection logic, including the length-
// validation error path, is unit-testable without touching the environment
// or a database.
func SelectServiceKeySecret(serviceKeySecretEnv string, sessionSigningKey []byte) (secret []byte, source string, err error) {
	if serviceKeySecretEnv == "" {
		return sessionSigningKey, ServiceKeySecretSourceFallback, nil
	}
	if len(serviceKeySecretEnv) < 32 {
		return nil, "", fmt.Errorf("SERVICE_KEY_SECRET must be at least 32 characters when set")
	}
	return []byte(serviceKeySecretEnv), ServiceKeySecretSourceDedicated, nil
}

const base62Chars = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// serviceKeyHMACLabel is the fixed, non-secret context string that domain-
// separates the service-key derivation from every other use of
// SESSION_SIGNING_KEY (currently: HS256 session JWT signing in auth/jwt.go).
//
// Reusing one secret as an HMAC key for two purposes is safe here because HMAC
// is a PRF: with a fixed key, HMAC(key, m1) and HMAC(key, m2) are
// computationally independent as long as the message spaces don't collide.
// Session JWTs are HMAC'd over a header+claims JSON blob that always differs
// from this label (it contains "typ":"JWT", per-user claims, expiry
// timestamps, ...); it can never equal the constant string
// "darkplum/service-key/v1". So an attacker who can influence or observe JWT
// inputs/outputs learns nothing about HMAC(secret, serviceKeyHMACLabel), and
// vice versa. This is the same "derive a subkey via HMAC over a fixed context
// string" pattern used by HKDF-Expand; we don't need a full HKDF-Extract step
// because SESSION_SIGNING_KEY is already a high-entropy, uniformly random
// server secret rather than a low-entropy input.
const serviceKeyHMACLabel = "darkplum/service-key/v1"

// serviceKeyBodyLen is the length of the base62 body after the "dp_" prefix,
// matching the format api-keys.go's generateAPIKey uses for user-created keys.
const serviceKeyBodyLen = 43

// deriveServiceKey deterministically computes the "_service" API key from the
// session-signing secret: every pod (and every replica) derives the identical
// key without coordination, so overlapping startups and N replicas never
// disagree, and rotating SESSION_SIGNING_KEY rotates the service key as a side
// effect. It is a pure function so it can be unit tested without a database.
func deriveServiceKey(secret []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(serviceKeyHMACLabel))
	digest := mac.Sum(nil)
	return "dp_" + base62EncodeDigest(digest, serviceKeyBodyLen)
}

// base62EncodeDigest deterministically encodes digest (big-endian bytes) as a
// base62 string of exactly length characters.
//
// A 256-bit HMAC-SHA256 digest interpreted as a big-endian integer usually
// base62-encodes to 43 or 44 characters (62^43 ~= 2^256.06), but a digest that
// happens to have enough leading zero bits encodes shorter — math/big drops
// leading zeros the same way fmt.Sprintf("%d", 007) would. Left-pad with the
// alphabet's zero digit ('0') before truncating so the output is always
// exactly `length` characters regardless of the digest's leading-zero-bit
// count, rather than occasionally emitting a short or out-of-range key.
func base62EncodeDigest(digest []byte, length int) string {
	n := new(big.Int).SetBytes(digest)
	if n.Sign() == 0 {
		return strings.Repeat(string(base62Chars[0]), length)
	}

	base := big.NewInt(int64(len(base62Chars)))
	mod := new(big.Int)
	encoded := make([]byte, 0, length+1)
	for n.Sign() > 0 {
		n.DivMod(n, base, mod)
		encoded = append(encoded, base62Chars[mod.Int64()])
	}
	// DivMod produces digits least-significant-first; reverse to
	// most-significant-first.
	for i, j := 0, len(encoded)-1; i < j; i, j = i+1, j-1 {
		encoded[i], encoded[j] = encoded[j], encoded[i]
	}

	for len(encoded) < length {
		encoded = append([]byte{base62Chars[0]}, encoded...)
	}
	return string(encoded[:length])
}

// BootstrapServiceKey ensures the deterministically-derived "_service" API key
// (see deriveServiceKey) is present and unrevoked in service_keys, and puts
// any other, now-superseded "_service" row (from a previous
// SESSION_SIGNING_KEY) into a 10-minute revocation grace window rather than
// revoking it immediately, so in-flight requests signed with the old key
// during a rolling restart don't fail. Returns the raw API key for the web
// service to use when proxying chat requests to the gateway.
func BootstrapServiceKey(ctx context.Context, pool *pgxpool.Pool, sessionSigningKey []byte) (string, error) {
	// Clean up legacy system user if it still exists from before the
	// service_keys table was introduced. ON DELETE CASCADE on api_keys.user_id
	// removes its api_keys rows automatically.
	if _, err := pool.Exec(ctx,
		`DELETE FROM users WHERE email = 'system@darkplum.internal'`,
	); err != nil {
		slog.WarnContext(ctx, "legacy system user cleanup failed", "err", err)
	}

	fullKey := deriveServiceKey(sessionSigningKey)
	prefix := fullKey[:11]
	sum := sha256.Sum256([]byte(fullKey))
	hash := hex.EncodeToString(sum[:])

	if _, err := pool.Exec(ctx, `
		UPDATE service_keys
		SET revoked_at = now() + interval '10 minutes'
		WHERE name = '_service' AND key_hash <> $1 AND revoked_at IS NULL
	`, hash); err != nil {
		return "", fmt.Errorf("supersede old service keys: %w", err)
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO service_keys (name, prefix, key_hash)
		VALUES ('_service', $1, $2)
		ON CONFLICT (key_hash) DO UPDATE SET revoked_at = NULL
	`, prefix, hash); err != nil {
		return "", fmt.Errorf("upsert service key: %w", err)
	}

	slog.InfoContext(ctx, "service api key bootstrapped", "prefix", prefix)
	return fullKey, nil
}
