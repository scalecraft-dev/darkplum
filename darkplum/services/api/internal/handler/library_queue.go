package handler

// The library_sync queue's write side. Every change that can make the bucket
// copy stale inserts or bumps a row in the SAME transaction as the change, so
// "committed in Postgres" and "queued for the bucket" cannot disagree. The
// worker (library_sync.go) is the only reader.
//
// With LIBRARY_STORE_URL unset the worker is off and rows simply accumulate
// (at most one per item or folder); they drain once a store is configured.

import (
	"context"
	"net/http"

	"github.com/jackc/pgx/v5/pgconn"
)

type queueExec interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

const queueBump = ` SET rev = library_sync.rev + 1, enqueued_at = now(), not_before = now(),
	attempts = 0, last_error = NULL, locked_until = NULL`

// enqueueItem marks an item's bucket copy stale.
func enqueueItem(ctx context.Context, q queueExec, itemID string) error {
	_, err := q.Exec(ctx, `INSERT INTO library_sync (item_id) VALUES ($1)
		ON CONFLICT (item_id) WHERE item_id IS NOT NULL DO UPDATE`+queueBump, itemID)
	return err
}

// enqueueFolder marks a folder's marker object stale.
func enqueueFolder(ctx context.Context, q queueExec, folderID string) error {
	_, err := q.Exec(ctx, `INSERT INTO library_sync (folder_id) VALUES ($1)
		ON CONFLICT (folder_id) WHERE folder_id IS NOT NULL DO UPDATE`+queueBump, folderID)
	return err
}

func nonNil(keys []*string) []string {
	out := []string{}
	for _, k := range keys {
		if k != nil && *k != "" {
			out = append(out, *k)
		}
	}
	return out
}

// enqueueItemDeleted queues the removal of a deleted item's objects: the
// worker deletes every key and purges all their versions (erasure runs
// through the queue, never fire-and-forget). With no key recorded there is
// nothing in the bucket; any queued row is dropped (an in-flight Put cannot
// have started: the worker records its pending key before writing).
func enqueueItemDeleted(ctx context.Context, q queueExec, itemID string, keys ...*string) error {
	ks := nonNil(keys)
	if len(ks) == 0 {
		_, err := q.Exec(ctx, `DELETE FROM library_sync WHERE item_id = $1`, itemID)
		return err
	}
	_, err := q.Exec(ctx, `INSERT INTO library_sync (item_id, stale_keys) VALUES ($1, $2)
		ON CONFLICT (item_id) WHERE item_id IS NOT NULL DO UPDATE`+queueBump+`,
		stale_keys = ARRAY(SELECT DISTINCT unnest(library_sync.stale_keys || EXCLUDED.stale_keys))`, itemID, ks)
	return err
}

// enqueueFolderDeleted queues removal of a deleted folder's marker.
func enqueueFolderDeleted(ctx context.Context, q queueExec, folderID string, key *string) error {
	ks := nonNil([]*string{key})
	if len(ks) == 0 {
		_, err := q.Exec(ctx, `DELETE FROM library_sync WHERE folder_id = $1`, folderID)
		return err
	}
	_, err := q.Exec(ctx, `INSERT INTO library_sync (folder_id, stale_keys) VALUES ($1, $2)
		ON CONFLICT (folder_id) WHERE folder_id IS NOT NULL DO UPDATE`+queueBump+`,
		stale_keys = ARRAY(SELECT DISTINCT unnest(library_sync.stale_keys || EXCLUDED.stale_keys))`, folderID, ks)
	return err
}

// libError is a failure with a stable code the UI shows by name.
type libError struct {
	status int
	code   string
	msg    string
}

func (e *libError) Error() string { return e.code }

var (
	errLibraryFull      = &libError{http.StatusUnprocessableEntity, "library_full", "your library holds the maximum number of items"}
	errSlugTaken        = &libError{http.StatusConflict, "slug_taken", "an item with that slug already exists there"}
	errSharedPathTaken  = &libError{http.StatusConflict, "shared_path_taken", "the organisation already has a file with that name in that folder"}
	errFolderExists     = &libError{http.StatusConflict, "folder_exists", "a folder with that name already exists"}
	errFolderInvalid    = &libError{http.StatusBadRequest, "folder_invalid", "folder names use lowercase letters, digits and hyphens (1-64 characters), at most 5 levels deep"}
	errFolderNotEmpty   = &libError{http.StatusConflict, "folder_not_empty", "the folder is not empty"}
	errFolderLimit      = &libError{http.StatusUnprocessableEntity, "folder_limit", "too many folders"}
	errFolderNotFound   = &libError{http.StatusNotFound, "folder_not_found", "folder not found"}
	errItemNotFound     = &libError{http.StatusNotFound, "not_found", "item not found"}
	errAdminRequired    = &libError{http.StatusForbidden, "admin_required", "admin access required"}
	errNoOwner          = &libError{http.StatusUnprocessableEntity, "item_has_no_owner", "the item has no owner to return it to"}
	errStoreUnavailable = &libError{http.StatusServiceUnavailable, "library_storage_unavailable", "the library's object store is not available"}
)
