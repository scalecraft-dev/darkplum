package handler

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

var uuidRe = regexp.MustCompile(
	`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`,
)

func isValidUUID(s string) bool {
	return uuidRe.MatchString(s)
}

// isUniqueViolation returns true for Postgres error code 23505.
func isUniqueViolation(err error) bool {
	if pgErr, ok := err.(*pgconn.PgError); ok {
		return pgErr.Code == "23505"
	}
	return false
}

// isForeignKeyViolation returns true for Postgres error code 23503.
func isForeignKeyViolation(err error) bool {
	if pgErr, ok := err.(*pgconn.PgError); ok {
		return pgErr.Code == "23503"
	}
	return false
}

// gatewayRefreshTimeout bounds the best-effort "policy changed, reload now"
// call below. The gateway re-reads policy every POLICY_REFRESH_SECONDS on
// its own; this only shortens the window in which an admin's save is not
// yet what the gateway enforces, so a slow or failed kick is logged, never
// surfaced.
const gatewayRefreshTimeout = 3 * time.Second

// kickGatewayPolicy asks the gateway to reload its policy snapshot right
// now (POST /v1/policy/refresh, service key only). Called after every write
// that the snapshot is built from -- model enable/disable, aliases, the org
// policy, per-principal policies -- so the next chat, the next effective-
// policy read, and the next lock-out preview all see the write at once
// instead of up to one refresh interval later.
func kickGatewayPolicy(ctx context.Context, client *http.Client, gatewayURL, serviceKey string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), gatewayRefreshTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gatewayURL+"/v1/policy/refresh", nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+serviceKey)
	resp, err := client.Do(req)
	if err != nil {
		slog.WarnContext(ctx, "gateway policy refresh kick failed; the gateway will pick the change up on its next interval", "err", err)
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		slog.WarnContext(ctx, "gateway policy refresh kick refused", "status", resp.StatusCode)
	}
}

// dedupeStrings returns in with repeats removed, first occurrence kept, so a
// list an admin typed twice into is stored (and shown back) once.
func dedupeStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}
