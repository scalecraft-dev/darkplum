package db

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

var bootstrapTestNameRe = regexp.MustCompile(`[^a-z0-9_]+`)

// setupBootstrapTestDB is the db-package twin of handler's setupTestDB: skips
// unless TEST_DATABASE_URL is set, then returns a pool scoped to a fresh,
// unique schema with migrations applied, cleaned up via t.Cleanup.
func setupBootstrapTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()

	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping DB integration test")
	}

	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	name := bootstrapTestNameRe.ReplaceAllString(strings.ToLower(t.Name()), "_")
	if len(name) > 40 {
		name = name[:40]
	}
	schema := "test_" + name + "_" + hex.EncodeToString(suffix)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	if _, err := admin.Exec(ctx, fmt.Sprintf(`CREATE SCHEMA "%s"`, schema)); err != nil {
		admin.Close()
		t.Fatalf("create schema %s: %v", schema, err)
	}
	admin.Close()

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open pool scoped to schema %s: %v", schema, err)
	}

	if err := RunMigrations(ctx, pool); err != nil {
		pool.Close()
		t.Fatalf("run migrations into schema %s: %v", schema, err)
	}

	t.Cleanup(func() {
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer dropCancel()
		if _, err := pool.Exec(dropCtx, fmt.Sprintf(`DROP SCHEMA IF EXISTS "%s" CASCADE`, schema)); err != nil {
			t.Logf("drop schema %s: %v", schema, err)
		}
		pool.Close()
	})

	return pool
}

// countValidServiceKeys returns how many "_service" rows in service_keys are
// currently unrevoked (revoked_at IS NULL OR in the future).
func countValidServiceKeys(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM service_keys
		WHERE name = '_service' AND (revoked_at IS NULL OR revoked_at > now())
	`).Scan(&n)
	if err != nil {
		t.Fatalf("count valid service keys: %v", err)
	}
	return n
}

// TestBootstrapServiceKey_CalledTwiceIsIdempotent verifies that bootstrapping
// twice with the same secret leaves exactly one valid derived key: the second
// call's INSERT ... ON CONFLICT (key_hash) DO UPDATE SET revoked_at = NULL
// path must not create a duplicate row or leave two live keys.
func TestBootstrapServiceKey_CalledTwiceIsIdempotent(t *testing.T) {
	pool := setupBootstrapTestDB(t)
	ctx := context.Background()
	secret := []byte("a-session-signing-key-at-least-32-chars")

	key1, err := BootstrapServiceKey(ctx, pool, secret)
	if err != nil {
		t.Fatalf("first BootstrapServiceKey: %v", err)
	}
	key2, err := BootstrapServiceKey(ctx, pool, secret)
	if err != nil {
		t.Fatalf("second BootstrapServiceKey: %v", err)
	}
	if key1 != key2 {
		t.Fatalf("expected the same derived key across calls, got %q and %q", key1, key2)
	}

	var total int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM service_keys WHERE name = '_service'`).Scan(&total); err != nil {
		t.Fatalf("count service_keys rows: %v", err)
	}
	if total != 1 {
		t.Fatalf("expected exactly one _service row after two identical bootstraps, got %d", total)
	}
	if got := countValidServiceKeys(t, ctx, pool); got != 1 {
		t.Fatalf("expected exactly one valid _service key, got %d", got)
	}
}

// TestBootstrapServiceKey_ConcurrentCallsLeaveExactlyOneValidKey exercises N
// replicas racing to bootstrap at once (the scenario the deterministic
// derivation is meant to make safe).
func TestBootstrapServiceKey_ConcurrentCallsLeaveExactlyOneValidKey(t *testing.T) {
	pool := setupBootstrapTestDB(t)
	ctx := context.Background()
	secret := []byte("a-session-signing-key-at-least-32-chars")

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	keys := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			keys[i], errs[i] = BootstrapServiceKey(ctx, pool, secret)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("BootstrapServiceKey[%d]: %v", i, err)
		}
		if keys[i] != keys[0] {
			t.Fatalf("BootstrapServiceKey[%d] = %q, want %q (all replicas must derive the same key)", i, keys[i], keys[0])
		}
	}

	if got := countValidServiceKeys(t, ctx, pool); got != 1 {
		t.Fatalf("expected exactly one valid _service key after %d concurrent bootstraps, got %d", n, got)
	}
}

// TestBootstrapServiceKey_RotatesOldKeyWithGraceWindow verifies that
// bootstrapping with a new secret supersedes the old "_service" row with a
// future revoked_at (grace window) rather than revoking it immediately or
// leaving two simultaneously-valid keys.
func TestBootstrapServiceKey_RotatesOldKeyWithGraceWindow(t *testing.T) {
	pool := setupBootstrapTestDB(t)
	ctx := context.Background()

	oldSecret := []byte("the-old-session-signing-key-32-chars!!!")
	newSecret := []byte("a-brand-new-session-signing-key-32-char")

	oldKey, err := BootstrapServiceKey(ctx, pool, oldSecret)
	if err != nil {
		t.Fatalf("bootstrap with old secret: %v", err)
	}
	newKey, err := BootstrapServiceKey(ctx, pool, newSecret)
	if err != nil {
		t.Fatalf("bootstrap with new secret: %v", err)
	}
	if oldKey == newKey {
		t.Fatalf("expected different secrets to derive different keys")
	}

	oldSum := sha256.Sum256([]byte(oldKey))
	newSum := sha256.Sum256([]byte(newKey))
	oldHash := hex.EncodeToString(oldSum[:])
	newHash := hex.EncodeToString(newSum[:])

	var oldRevokedAt *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT revoked_at FROM service_keys WHERE key_hash = $1`, oldHash,
	).Scan(&oldRevokedAt); err != nil {
		t.Fatalf("fetch old key row: %v", err)
	}
	if oldRevokedAt == nil {
		t.Fatal("expected the old service key to have a non-NULL revoked_at after rotation")
	}
	if !oldRevokedAt.After(time.Now()) {
		t.Fatalf("expected old service key's revoked_at (%v) to be in the future (grace window)", oldRevokedAt)
	}
	if !oldRevokedAt.Before(time.Now().Add(11 * time.Minute)) {
		t.Fatalf("old service key's revoked_at (%v) is further out than the ~10-minute grace window", oldRevokedAt)
	}

	var newRevokedAt *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT revoked_at FROM service_keys WHERE key_hash = $1`, newHash,
	).Scan(&newRevokedAt); err != nil {
		t.Fatalf("fetch new key row: %v", err)
	}
	if newRevokedAt != nil {
		t.Fatalf("expected the new service key to be unrevoked, got revoked_at = %v", newRevokedAt)
	}

	// Exactly one row is unconditionally valid right now; the old one is only
	// still valid because it's inside its grace window, which countValidServiceKeys
	// correctly counts as 2 during the overlap. Assert the overlap explicitly instead.
	if got := countValidServiceKeys(t, ctx, pool); got != 2 {
		t.Fatalf("expected old+new key both valid during the grace window, got %d valid keys", got)
	}
}
