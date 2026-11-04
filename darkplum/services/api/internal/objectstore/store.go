// Package objectstore is the cloud-neutral interface the library's bucket
// copy is written through. S3 is the first implementation (s3.go); GCS slots
// in behind the same interface. There is no filesystem implementation on
// purpose (a traversal surface, and an untested path in production), and no
// Copy, conditional write or presigned URL: exports stream through the api.
//
// Keys at this interface are relative to the store's configured prefix
// (normally "library/v1/"); the implementation adds it, so callers and
// libkey never see the prefix.
//
// Errors never carry a key, prefix or SDK message: keys hold user folder
// names and must not reach logs. Use Code(err) for log fields.
package objectstore

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is returned by Head and Get for a missing object.
var ErrNotFound = errors.New("objectstore: not found")

// Meta is user metadata (x-amz-meta-* on S3). Keys are stored lowercase.
type Meta map[string]string

// Info describes one object.
type Info struct {
	Key          string // relative to the store prefix
	Size         int64
	LastModified time.Time
	ETag         string
	Meta         Meta // Head and Get only; List leaves it nil
}

// Store is what the sync worker and Sync-from-bucket need.
type Store interface {
	// Put writes (or replaces) an object. Writing the same bytes twice is a
	// no-op in effect, which is what makes the worker idempotent.
	Put(ctx context.Context, key string, body []byte, meta Meta) error
	// Head returns ErrNotFound for a missing object.
	Head(ctx context.Context, key string) (Info, error)
	// Get returns the object's bytes; ErrNotFound if missing.
	Get(ctx context.Context, key string) ([]byte, Info, error)
	// Delete removes the current version. A missing object is not an error.
	Delete(ctx context.Context, key string) error
	// PurgeVersions removes every version and delete marker of exactly key
	// (erasure). A missing object is not an error.
	PurgeVersions(ctx context.Context, key string) error
	// List returns up to limit objects under prefix in key order, and a
	// cursor for the next page ("" when done).
	List(ctx context.Context, prefix, cursor string, limit int) ([]Info, string, error)
	// Bucket names the bucket for display (never a secret).
	Bucket() string
	// URI is the s3://bucket/prefix+key form of a key, for the UI.
	URI(key string) string
}

// Error is a store failure reduced to what is safe to log.
type Error struct {
	Op        string // put, get, head, delete, purge, list
	Code      string // provider error code, e.g. AccessDenied
	Status    int    // HTTP status when known
	RequestID string
}

func (e *Error) Error() string {
	s := "objectstore " + e.Op + " failed"
	if e.Code != "" {
		s += ": " + e.Code
	}
	return s
}

// Code returns the provider error code of err, or "error".
func Code(err error) string {
	var se *Error
	if errors.As(err, &se) && se.Code != "" {
		return se.Code
	}
	if errors.Is(err, ErrNotFound) {
		return "NotFound"
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "Timeout"
	}
	return "error"
}
