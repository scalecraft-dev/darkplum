package handler

// The write-through worker that keeps the bucket a complete copy of the
// library, and the hourly reconciler.
//
// Postgres is canonical. Every library change queues a library_sync row in
// its own transaction (library_queue.go); this worker drains the queue and
// RE-APPLIES THE CURRENT STATE of the item or folder (it never replays
// operations), so every step is idempotent and a retry after a crash is
// safe:
//
//  1. record s3_pending_key (the key a Put is about to create),
//  2. Put the desired key with metadata,
//  3. Delete the keys the item used to live at and purge every version of
//     them (Put-then-Delete: a move briefly leaves both copies, which is
//     normal; the new key starts a fresh history),
//  4. record s3_key / s3_synced_sha / s3_obj_sha, clear the queue row if no
//     one re-queued it meanwhile (rev).
//
// Several api replicas run this: rows are claimed with FOR UPDATE SKIP
// LOCKED and a short lease. Nothing here reads the bucket on the chat path.
//
// LOGGING: item ids, operation names, provider error codes, status and
// request id only. Never a key, prefix, name, path or an SDK error string:
// keys carry user folder names.

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/scalecraft-dev/darkplum/api/internal/libkey"
	"github.com/scalecraft-dev/darkplum/api/internal/objectstore"
)

const (
	syncSentinelKey    = ".darkplum-sentinel"
	syncLease          = 2 * time.Minute
	syncBatch          = 10
	syncOpTimeout      = 30 * time.Second
	syncIdleSleep      = 2 * time.Second
	syncHealthEvery    = 60 * time.Second
	syncStatsEvery     = 60 * time.Second
	syncReconcileEvery = time.Hour
	reconcileLockKey   = "darkplum_library_reconcile"
)

// Health is the cached result of the sentinel HEAD. It is deliberately NOT
// wired into /health: a bucket outage must not take the api out of rotation
// (chat does not depend on the bucket).
type SyncHealth struct {
	Reachable bool
	CheckedAt time.Time
	Code      string
}

// LibrarySyncer drains library_sync into the object store.
type LibrarySyncer struct {
	db    *pgxpool.Pool
	store objectstore.Store

	mu            sync.Mutex
	health        SyncHealth
	lastReconcile time.Time
	foreign       int
}

func NewLibrarySyncer(db *pgxpool.Pool, store objectstore.Store) *LibrarySyncer {
	return &LibrarySyncer{db: db, store: store}
}

func (s *LibrarySyncer) lock()   { s.mu.Lock() }
func (s *LibrarySyncer) unlock() { s.mu.Unlock() }

// Health returns the last sentinel result (zero value before the first check).
func (s *LibrarySyncer) Health() SyncHealth {
	s.lock()
	defer s.unlock()
	return s.health
}

// ForeignObjects is how many objects the last reconcile found that Postgres
// never wrote (reported, never deleted).
func (s *LibrarySyncer) ForeignObjects() (n int, at time.Time) {
	s.lock()
	defer s.unlock()
	return s.foreign, s.lastReconcile
}

// syncBackoff is the delay before attempt n+1 after n failures.
func syncBackoff(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	d := 5 * time.Second
	for i := 1; i < attempts && d < 15*time.Minute; i++ {
		d *= 2
	}
	if d > 15*time.Minute {
		d = 15 * time.Minute
	}
	return d
}

// Run blocks until ctx is done.
func (s *LibrarySyncer) Run(ctx context.Context) {
	slog.InfoContext(ctx, "library sync worker started")
	s.CheckHealth(ctx)
	nextHealth := time.Now().Add(syncHealthEvery)
	nextStats := time.Now().Add(syncStatsEvery)
	nextReconcile := time.Now().Add(30 * time.Second)
	for ctx.Err() == nil {
		n := s.RunOnce(ctx)
		now := time.Now()
		if now.After(nextHealth) {
			s.CheckHealth(ctx)
			nextHealth = now.Add(syncHealthEvery)
		}
		if now.After(nextStats) {
			s.logStats(ctx)
			nextStats = now.Add(syncStatsEvery)
		}
		if now.After(nextReconcile) {
			s.Reconcile(ctx)
			nextReconcile = now.Add(syncReconcileEvery)
		}
		if n == 0 {
			select {
			case <-ctx.Done():
			case <-time.After(syncIdleSleep):
			}
		}
	}
}

type syncJob struct {
	id       int64
	itemID   *string
	folderID *string
	stale    []string
	rev      int64
	attempts int
}

// claim leases up to n due rows.
func (s *LibrarySyncer) claim(ctx context.Context, n int) ([]syncJob, error) {
	rows, err := s.db.Query(ctx, `
		UPDATE library_sync q SET locked_until = now() + $2::interval
		WHERE q.id IN (
			SELECT id FROM library_sync
			WHERE not_before <= now() AND (locked_until IS NULL OR locked_until < now())
			ORDER BY enqueued_at LIMIT $1 FOR UPDATE SKIP LOCKED)
		RETURNING q.id, q.item_id::text, q.folder_id::text, q.stale_keys, q.rev, q.attempts`, n, syncLease.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []syncJob
	for rows.Next() {
		var j syncJob
		if err := rows.Scan(&j.id, &j.itemID, &j.folderID, &j.stale, &j.rev, &j.attempts); err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

// RunOnce claims and processes one batch; returns how many rows it handled.
func (s *LibrarySyncer) RunOnce(ctx context.Context) int {
	jobs, err := s.claim(ctx, syncBatch)
	if err != nil {
		if ctx.Err() == nil {
			slog.ErrorContext(ctx, "library sync claim failed")
		}
		return 0
	}
	for _, j := range jobs {
		start := time.Now()
		opCtx, cancel := context.WithTimeout(ctx, syncOpTimeout)
		var perr error
		label := ""
		switch {
		case j.itemID != nil:
			label = *j.itemID
			perr = s.syncItem(opCtx, j)
		case j.folderID != nil:
			label = *j.folderID
			perr = s.syncFolder(opCtx, j)
		}
		cancel()
		switch {
		case errors.Is(perr, errBucketChanged):
			// Not a failure: parked until Sync from bucket reports the file.
			slog.InfoContext(ctx, "library sync held back: object changed in the bucket", "id", label)
			_, _ = s.db.Exec(ctx, `UPDATE library_sync SET last_error = 'bucket_changed', not_before = now() + $3::interval,
				locked_until = NULL WHERE id = $1 AND rev = $2`, j.id, j.rev, bucketChangedRetry.String())
		case perr != nil:
			s.fail(ctx, j, label, perr, time.Since(start))
		}
	}
	return len(jobs)
}

func (s *LibrarySyncer) fail(ctx context.Context, j syncJob, id string, err error, took time.Duration) {
	code := objectstore.Code(err)
	var le *libError
	if errors.As(err, &le) {
		code = le.code
	}
	args := []any{"id", id, "target", map[bool]string{true: "item", false: "folder"}[j.itemID != nil],
		"code", code, "attempts", j.attempts + 1, "bucket", s.store.Bucket(), "duration_ms", took.Milliseconds()}
	var se *objectstore.Error
	if errors.As(err, &se) {
		args = append(args, "op", se.Op, "status", se.Status, "request_id", se.RequestID)
	}
	slog.WarnContext(ctx, "library sync failed", args...)
	_, uerr := s.db.Exec(ctx, `UPDATE library_sync SET attempts = attempts + 1, last_error = $3,
		not_before = now() + $4::interval, locked_until = NULL WHERE id = $1 AND rev = $2`,
		j.id, j.rev, code, syncBackoff(j.attempts+1).String())
	if uerr != nil && ctx.Err() == nil {
		slog.ErrorContext(ctx, "library sync could not record failure", "id", id)
	}
}

func (s *LibrarySyncer) done(ctx context.Context, j syncJob) error {
	_, err := s.db.Exec(ctx, `DELETE FROM library_sync WHERE id = $1 AND rev = $2`, j.id, j.rev)
	return err
}

// removeKeys deletes keys and purges every version of each (erasure).
func (s *LibrarySyncer) removeKeys(ctx context.Context, keys []string) error {
	for _, k := range keys {
		if err := s.store.Delete(ctx, k); err != nil {
			return err
		}
		if err := s.store.PurgeVersions(ctx, k); err != nil {
			return err
		}
	}
	return nil
}

func (s *LibrarySyncer) syncItem(ctx context.Context, j syncJob) error {
	id := *j.itemID
	var pending *string
	r, err := scanLibraryRow(s.db.QueryRow(ctx, `SELECT `+librarySelectCols+`, li.s3_pending_key`+libraryFrom+` WHERE li.id = $1`, id), &pending)
	if errors.Is(err, pgx.ErrNoRows) {
		// Deleted: remove what the delete recorded, every version.
		if err := s.removeKeys(ctx, j.stale); err != nil {
			return err
		}
		return s.done(ctx, j)
	}
	if err != nil {
		return err
	}
	desired, err := keyForRow(r)
	if err != nil {
		// A name that cannot be a key (reserved device name, too deep):
		// surfaced as a code on the queue row; the item stays in Postgres.
		return &libError{code: "key_invalid"}
	}
	dk := desired.String()
	// A key left by an earlier half-finished attempt that the item no longer
	// wants and never committed.
	if pending != nil && *pending != dk && (r.s3Key == nil || *r.s3Key != *pending) {
		if err := s.removeKeys(ctx, []string{*pending}); err != nil {
			return err
		}
	}
	// Never overwrite a change made in the bucket. If the object we wrote last
	// time is still at this key but its bytes are no longer the ones we
	// recorded, someone edited or replaced it there: flag it and leave the row
	// queued until Sync from bucket has reported it (a missing object is not
	// a change and is simply written again).
	if r.s3Key != nil && *r.s3Key == dk && r.objSHA != nil {
		changed, cerr := s.bucketChangedSince(ctx, dk, *r.objSHA)
		if cerr != nil {
			return cerr
		}
		if changed {
			if _, err := s.db.Exec(ctx, `UPDATE library_items SET bucket_changed_at = COALESCE(bucket_changed_at, now()) WHERE id = $1`, id); err != nil {
				return err
			}
			return errBucketChanged
		}
	}
	tag, err := s.db.Exec(ctx, `UPDATE library_items SET s3_pending_key = $2 WHERE id = $1`, id, dk)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return nil // deleted meanwhile; its own queue row removes what exists
	}
	_, _, body := renderExport(r.toWire(""))
	objSHA := objectSHA256(body)
	if err := s.store.Put(ctx, dk, body, objectstore.Meta{"darkplum-item-id": id, "darkplum-sha256": objSHA}); err != nil {
		return err
	}
	if r.s3Key != nil && *r.s3Key != dk {
		// The old key is removed with every version it holds: a rename or move
		// starts a new history at the new key. Leaving noncurrent versions
		// behind at keys Postgres no longer knows would make "delete" and
		// "unshare" untrue (nothing could ever name them for erasure).
		if err := s.removeKeys(ctx, []string{*r.s3Key}); err != nil {
			return err
		}
	}
	if _, err := s.db.Exec(ctx, `UPDATE library_items SET s3_key = $2, s3_pending_key = NULL, s3_synced_sha = $3, s3_obj_sha = $4, s3_synced_at = now(), bucket_changed_at = NULL WHERE id = $1`,
		id, dk, r.sha, objSHA); err != nil {
		return err
	}
	return s.done(ctx, j)
}

// errBucketChanged parks a queue row instead of failing it.
var errBucketChanged = errors.New("object changed in the bucket")

const bucketChangedRetry = 15 * time.Minute

// bucketChangedSince reports whether the object at key exists with bytes
// other than the recorded hash. The darkplum-sha256 metadata is the
// quick answer; when it is missing or different (a re-upload drops it) the
// bytes themselves decide, so an object Sync just adopted, or re-uploaded
// identical, is not mistaken for a foreign edit.
func (s *LibrarySyncer) bucketChangedSince(ctx context.Context, key, recorded string) (bool, error) {
	info, err := s.store.Head(ctx, key)
	if errors.Is(err, objectstore.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Meta["darkplum-sha256"] == recorded {
		return false, nil
	}
	b, _, err := s.store.Get(ctx, key)
	if errors.Is(err, objectstore.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return objectSHA256(b) != recorded, nil
}

func (s *LibrarySyncer) syncFolder(ctx context.Context, j syncJob) error {
	id := *j.folderID
	var scope, path string
	var owner *string
	var oldKey *string
	var person *string
	err := s.db.QueryRow(ctx, `SELECT f.scope, f.path, f.owner_user_id::text, f.s3_key, u.library_slug
		FROM library_folders f LEFT JOIN users u ON u.id = f.owner_user_id WHERE f.id = $1`, id).Scan(&scope, &path, &owner, &oldKey, &person)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := s.removeKeys(ctx, j.stale); err != nil {
			return err
		}
		return s.done(ctx, j)
	}
	if err != nil {
		return err
	}
	var key libkey.Key
	if scope == "org" {
		key, err = libkey.ForFolder(libkey.ScopeOrg, "", path)
	} else if person != nil {
		key, err = libkey.ForFolder(libkey.ScopeUser, *person, path)
	} else {
		err = libkey.ErrInvalid
	}
	if err != nil {
		return &libError{code: "key_invalid"}
	}
	k := key.String()
	if _, err := s.db.Exec(ctx, `UPDATE library_folders SET s3_key = $2 WHERE id = $1`, id, k); err != nil {
		return err
	}
	if err := s.store.Put(ctx, k, nil, objectstore.Meta{"darkplum-folder-id": id}); err != nil {
		return err
	}
	if oldKey != nil && *oldKey != k {
		if err := s.removeKeys(ctx, []string{*oldKey}); err != nil {
			return err
		}
	}
	return s.done(ctx, j)
}

// CheckHealth refreshes the cached sentinel HEAD (creating the sentinel on
// first use).
func (s *LibrarySyncer) CheckHealth(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := s.store.Head(cctx, syncSentinelKey)
	if errors.Is(err, objectstore.ErrNotFound) {
		err = s.store.Put(cctx, syncSentinelKey, nil, objectstore.Meta{"darkplum-sentinel": "1"})
	}
	h := SyncHealth{Reachable: err == nil, CheckedAt: time.Now()}
	if err != nil {
		h.Code = objectstore.Code(err)
	}
	s.lock()
	prev := s.health
	s.health = h
	s.unlock()
	if prev.CheckedAt.IsZero() || prev.Reachable != h.Reachable {
		slog.InfoContext(ctx, "library object store health", "reachable", h.Reachable, "code", h.Code, "bucket", s.store.Bucket())
	}
}

// QueueStats is the queue as the admin status route and the minute log see it.
type QueueStats struct {
	Depth, Failing      int
	OldestQueuedSeconds int
}

// Stats reads the queue. Failing counts rows that have failed five times or more.
func (s *LibrarySyncer) Stats(ctx context.Context) (QueueStats, error) {
	var st QueueStats
	var oldest *float64
	if err := s.db.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE attempts >= 5),
		EXTRACT(EPOCH FROM now() - min(enqueued_at))::float8 FROM library_sync`).Scan(&st.Depth, &st.Failing, &oldest); err != nil {
		return st, err
	}
	if oldest != nil {
		st.OldestQueuedSeconds = int(*oldest)
	}
	return st, nil
}

func (s *LibrarySyncer) logStats(ctx context.Context) {
	st, err := s.Stats(ctx)
	if err != nil {
		return
	}
	slog.InfoContext(ctx, "library sync queue", "queue_depth", st.Depth, "failing", st.Failing, "oldest_queued_seconds", st.OldestQueuedSeconds)
}

// Reconcile makes the bucket copy honest, once an hour, one replica at a time
// (advisory lock):
//   - items whose recorded content differs from what was synced are re-queued;
//   - every synced item's object is HEADed (bounded concurrency): a missing
//     one is re-queued, so a file deleted in the console comes back; one whose
//     darkplum-sha256 metadata is missing or differs is flagged
//     (bucket_changed_at) and NOT overwritten: that is an edit made in the
//     bucket, adopted or reported by "Sync from bucket";
//   - objects Postgres never wrote are counted and logged, never deleted.
//
// It never deletes anything.
func (s *LibrarySyncer) Reconcile(ctx context.Context) {
	conn, err := s.db.Acquire(ctx)
	if err != nil {
		return
	}
	defer conn.Release()
	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtext($1))`, reconcileLockKey).Scan(&got); err != nil || !got {
		return
	}
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(hashtext($1))`, reconcileLockKey)
	}()

	var items, folders int64
	if tag, err := conn.Exec(ctx, `INSERT INTO library_sync (item_id)
		SELECT id FROM library_items WHERE s3_synced_sha IS DISTINCT FROM content_sha256 ON CONFLICT DO NOTHING`); err == nil {
		items = tag.RowsAffected()
	}
	if tag, err := conn.Exec(ctx, `INSERT INTO library_sync (folder_id)
		SELECT id FROM library_folders WHERE s3_key IS NULL ON CONFLICT DO NOTHING`); err == nil {
		folders = tag.RowsAffected()
	}

	verified, requeued := s.verifyObjects(ctx, conn)
	items += requeued

	known := map[string]bool{syncSentinelKey: true}
	krows, err := conn.Query(ctx, `
		SELECT s3_key FROM library_items WHERE s3_key IS NOT NULL
		UNION SELECT s3_pending_key FROM library_items WHERE s3_pending_key IS NOT NULL
		UNION SELECT s3_key FROM library_folders WHERE s3_key IS NOT NULL
		UNION SELECT unnest(stale_keys) FROM library_sync`)
	if err == nil {
		for krows.Next() {
			var k string
			if krows.Scan(&k) == nil {
				known[k] = true
			}
		}
		krows.Close()
	}
	foreign, cursor, scanned := 0, "", 0
	for scanned < 100000 {
		objs, next, lerr := s.store.List(ctx, "", cursor, 1000)
		if lerr != nil {
			slog.WarnContext(ctx, "library reconcile list failed", "code", objectstore.Code(lerr), "bucket", s.store.Bucket())
			foreign = -1
			break
		}
		for _, o := range objs {
			scanned++
			if !known[o.Key] {
				foreign++
			}
		}
		if next == "" {
			break
		}
		cursor = next
	}
	s.lock()
	s.lastReconcile = time.Now()
	if foreign >= 0 {
		s.foreign = foreign
	}
	s.unlock()
	slog.InfoContext(ctx, "library reconcile", "requeued_items", items, "requeued_folders", folders, "verified_objects", verified, "foreign_objects", foreign, "foreign_dry_run", true)
}

const (
	verifyConcurrency = 8
	verifyMaxItems    = 20000
)

// verifyObjects HEADs the recorded key of every synced, not-already-queued
// item. A MISSING object is re-queued (a file deleted in the console comes
// back); an object that exists but whose darkplum-sha256 metadata is missing
// or differs is only recorded (bucket_changed_at): it is somebody's edit and
// is never overwritten until Sync from bucket has reported it. A HEAD error that
// is not "not found" (an outage, a permission problem) re-queues nothing: it
// says nothing about the object.
func (s *LibrarySyncer) verifyObjects(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}) (verified, requeued int64) {
	rows, err := q.Query(ctx, `SELECT li.id::text, li.s3_key, COALESCE(li.s3_obj_sha, '') FROM library_items li
		WHERE li.s3_synced_sha IS NOT NULL AND li.s3_key IS NOT NULL
		  AND NOT EXISTS (SELECT 1 FROM library_sync x WHERE x.item_id = li.id)
		ORDER BY li.s3_synced_at NULLS FIRST LIMIT $1`, verifyMaxItems)
	if err != nil {
		return 0, 0
	}
	type cand struct{ id, key, sha string }
	var cands []cand
	for rows.Next() {
		var c cand
		if rows.Scan(&c.id, &c.key, &c.sha) == nil {
			cands = append(cands, c)
		}
	}
	rows.Close()

	var mu sync.Mutex
	var stale, changed []string
	sem := make(chan struct{}, verifyConcurrency)
	var wg sync.WaitGroup
	for _, c := range cands {
		if ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(c cand) {
			defer wg.Done()
			defer func() { <-sem }()
			hctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			info, err := s.store.Head(hctx, c.key)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case errors.Is(err, objectstore.ErrNotFound):
				stale = append(stale, c.id)
			case err != nil:
			default:
				verified++
				if info.Meta["darkplum-sha256"] != c.sha {
					changed = append(changed, c.id)
				}
			}
		}(c)
	}
	wg.Wait()
	// Exists but is not what we wrote: record it, never overwrite it.
	if len(changed) > 0 {
		_, _ = q.Exec(ctx, `UPDATE library_items SET bucket_changed_at = COALESCE(bucket_changed_at, now()) WHERE id = ANY($1::uuid[])`, changed)
	}
	for _, id := range stale {
		if tag, err := q.Exec(ctx, `INSERT INTO library_sync (item_id) VALUES ($1) ON CONFLICT DO NOTHING`, id); err == nil {
			requeued += tag.RowsAffected()
		}
	}
	return verified, requeued
}

// AdminStatus handles GET /api/v1/admin/library/status (admin): is the
// bucket reachable (cached sentinel HEAD), how deep and old is the sync
// queue, how many rows keep failing, when did the reconciler last run and
// how many items were found edited in the bucket (changed_in_bucket), and how
// many objects does the bucket hold that Postgres never wrote. No keys,
// names or paths are returned.
func (h *LibraryHandler) AdminStatus(c *gin.Context) {
	ctx := c.Request.Context()
	out := gin.H{"enabled": h.store != nil, "reachable": false, "bucket": "", "queue_depth": 0, "failing": 0,
		"oldest_queued_seconds": 0, "last_reconcile_at": nil, "foreign_objects": 0, "changed_in_bucket": 0}
	if h.store != nil {
		out["bucket"] = h.store.Bucket()
	}
	if h.syncer != nil {
		hl := h.syncer.Health()
		out["reachable"] = hl.Reachable
		if hl.Code != "" {
			out["error_code"] = hl.Code
		}
		if !hl.CheckedAt.IsZero() {
			out["checked_at"] = hl.CheckedAt
		}
		n, at := h.syncer.ForeignObjects()
		out["foreign_objects"] = n
		if !at.IsZero() {
			out["last_reconcile_at"] = at
		}
	}
	// The queue exists (and accumulates) with or without a store.
	var depth, failing int
	var oldest *float64
	if err := h.db.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE attempts >= 5),
		EXTRACT(EPOCH FROM now() - min(enqueued_at))::float8 FROM library_sync`).Scan(&depth, &failing, &oldest); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "library status failed"})
		return
	}
	out["queue_depth"], out["failing"] = depth, failing
	var changedN int
	if err := h.db.QueryRow(ctx, `SELECT count(*) FROM library_items WHERE bucket_changed_at IS NOT NULL`).Scan(&changedN); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "library status failed"})
		return
	}
	out["changed_in_bucket"] = changedN
	if oldest != nil {
		out["oldest_queued_seconds"] = int(*oldest)
	}
	c.JSON(http.StatusOK, out)
}
