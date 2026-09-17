package db

import (
	"context"
	"crypto/rand"
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

const base62Chars = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// BootstrapServiceKey ensures a service API key exists in the service_keys
// table. Returns the raw API key for the web service to use when proxying chat
// requests to the gateway. Rotates the key on every startup.
func BootstrapServiceKey(ctx context.Context, pool *pgxpool.Pool) (string, error) {
	// Clean up legacy system user if it still exists from before the
	// service_keys table was introduced. ON DELETE CASCADE on api_keys.user_id
	// removes its api_keys rows automatically.
	if _, err := pool.Exec(ctx,
		`DELETE FROM users WHERE email = 'system@darkplum.internal'`,
	); err != nil {
		slog.WarnContext(ctx, "legacy system user cleanup failed", "err", err)
	}

	_, _ = pool.Exec(ctx, `
		UPDATE service_keys SET revoked_at = now()
		WHERE name = '_service' AND revoked_at IS NULL
	`)

	rawKey, err := generateBase62(43)
	if err != nil {
		return "", fmt.Errorf("generate service key: %w", err)
	}
	fullKey := "dp_" + rawKey
	prefix := fullKey[:11]
	sum := sha256.Sum256([]byte(fullKey))
	hash := hex.EncodeToString(sum[:])

	_, err = pool.Exec(ctx, `
		INSERT INTO service_keys (name, prefix, key_hash)
		VALUES ('_service', $1, $2)
	`, prefix, hash)
	if err != nil {
		return "", fmt.Errorf("insert service key: %w", err)
	}

	slog.InfoContext(ctx, "service api key bootstrapped", "prefix", prefix)
	return fullKey, nil
}

func generateBase62(length int) (string, error) {
	result := make([]byte, length)
	base := big.NewInt(int64(len(base62Chars)))
	for i := range result {
		n, err := rand.Int(rand.Reader, base)
		if err != nil {
			return "", err
		}
		result[i] = base62Chars[n.Int64()]
	}
	return string(result), nil
}
