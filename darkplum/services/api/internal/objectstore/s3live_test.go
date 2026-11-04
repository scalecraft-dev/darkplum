//go:build s3live

package objectstore

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// Run the contract against a real bucket (or MinIO):
//
//	LIBRARY_STORE_URL=s3://bucket/some-test-prefix/ [LIBRARY_S3_ENDPOINT=... (path-style is implied) LIBRARY_S3_ACCESS_KEY_ID=... LIBRARY_S3_SECRET_ACCESS_KEY=...] \
//	  go test -tags s3live ./internal/objectstore/
//
// The bucket must be versioned for the purge assertions to mean anything.
func TestS3LiveContract(t *testing.T) {
	cfg := S3ConfigFromEnv()
	if cfg.URL == "" {
		t.Skip("LIBRARY_STORE_URL not set")
	}
	s, err := NewS3(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	prefix := fmt.Sprintf("contract-%d-%d/", os.Getpid(), time.Now().UnixNano())
	runContract(t, s, prefix)
}
