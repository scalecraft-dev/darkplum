package commands

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"
)

// TestCatalogResolvesOnHuggingFace is the scheduled catalog check
// (.github/workflows/catalog-sha.yml): every reviewed entry must still be
// public, still ship safetensors, and its pinned SHA must still resolve (a force
// pushed or deleted revision would fail make deploy at model download time).
// It calls huggingface.co, so it only runs with DARKPLUM_CHECK_CATALOG=1.
func TestCatalogResolvesOnHuggingFace(t *testing.T) {
	if os.Getenv("DARKPLUM_CHECK_CATALOG") != "1" {
		t.Skip("set DARKPLUM_CHECK_CATALOG=1 to check the catalog against huggingface.co")
	}
	c := newHFClient()
	for _, m := range modelCatalog {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		info, err := c.Lookup(ctx, m.ID)
		if err != nil {
			cancel()
			t.Errorf("%s: %v", m.ID, err)
			continue
		}
		if !info.Safetensors {
			t.Errorf("%s: no safetensors weights any more", m.ID)
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/models/"+m.ID+"/revision/"+m.Revision, nil)
		resp, err := c.http.Do(req)
		cancel()
		if err != nil {
			t.Errorf("%s: %v", m.ID, err)
			continue
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: pinned revision %s answers HTTP %d", m.ID, m.Revision, resp.StatusCode)
		}
		if info.SHA != m.Revision {
			t.Logf("note: %s main moved to %s (pinned %s still resolves); review before bumping", m.ID, info.SHA, m.Revision)
		}
	}
}
