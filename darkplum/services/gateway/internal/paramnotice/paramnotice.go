// Package paramnotice writes the admin audit event the first time a given
// principal has a given request parameter dropped by the gateway (GitHub #3).
//
// Dedupe is durable and shared across gateway replicas: the first insert into
// param_drop_notices for (principal_kind, principal_id, param) wins and, in
// the same statement, writes one activity_events row ("inference.param_dropped").
// A replica restart or a second replica therefore never repeats an event. An
// in-process set only saves the round trip for pairs already known to exist.
//
// The request path never waits on this: Note* enqueues and returns, and a full
// queue just skips (the pair is retried on the principal's next request).
package paramnotice

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/scalecraft-dev/darkplum/gateway/internal/chatparams"
)

// EventType is activity_events.event_type of the audit event. The api's
// activity package declares the same string.
const EventType = "inference.param_dropped"

// maxPerPrincipal bounds the distinct UNKNOWN keys noticed per principal: those
// names are chosen by the caller. A registry parameter name is a bounded set
// and is always audited, whatever the caller has sent before.
const maxPerPrincipal = 100

const (
	queueSize     = 256
	seenCap       = 20000
	backoffOnFail = time.Minute
)

var registryList = chatparams.ParamNames()

var registryNames = func() map[string]bool {
	m := map[string]bool{}
	for _, n := range registryList {
		m[n] = true
	}
	return m
}()

type item struct {
	kind, id, param, backend, model string
}

// Notifier implements handler.DropNotifier.
type Notifier struct {
	pool *pgxpool.Pool
	q    chan item

	mu    sync.Mutex
	seen  map[string]struct{}
	until time.Time // do not write before this (table missing / DB error)
	warn  bool
}

// New starts the writer goroutine.
func New(pool *pgxpool.Pool) *Notifier {
	n := &Notifier{pool: pool, q: make(chan item, queueSize), seen: map[string]struct{}{}}
	go n.run()
	return n
}

// NoteDrops records that params were dropped for the principal. Never blocks.
func (n *Notifier) NoteDrops(principalKind, principalID, backend, model string, params []string) {
	for _, p := range params {
		key := principalKind + "|" + principalID + "|" + p
		n.mu.Lock()
		_, known := n.seen[key]
		backoff := time.Now().Before(n.until)
		n.mu.Unlock()
		if known || backoff {
			continue
		}
		select {
		case n.q <- item{principalKind, principalID, p, backend, model}:
		default:
		}
	}
}

func (n *Notifier) run() {
	for it := range n.q {
		n.write(it)
	}
}

const insertSQL = `
WITH ins AS (
	INSERT INTO param_drop_notices (principal_kind, principal_id, param)
	SELECT $1::text, $2::uuid, $3::text
	WHERE $7::boolean OR (SELECT count(*) FROM param_drop_notices
		WHERE principal_kind = $1::text AND principal_id = $2::uuid AND param <> ALL($8::text[])) < $5
	ON CONFLICT DO NOTHING
	RETURNING 1
)
INSERT INTO activity_events (event_type, target_type, target_id, detail)
SELECT $6::text, $1::text, $2::uuid, $4::jsonb FROM ins`

func (n *Notifier) write(it item) {
	detail, _ := json.Marshal(map[string]any{
		"param":     it.param,
		"action":    "dropped",
		"backend":   it.backend,
		"model":     it.model,
		"note":      "the gateway accepted this request parameter but did not pass it to the backend; names only, never the value",
		"visible":   "X-Darkplum-Dropped-Params response header, inference_events.dropped_params",
		"principal": it.kind,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := n.pool.Exec(ctx, insertSQL, it.kind, it.id, it.param, string(detail), maxPerPrincipal, EventType, registryNames[it.param], registryList)
	n.mu.Lock()
	defer n.mu.Unlock()
	if err != nil {
		n.until = time.Now().Add(backoffOnFail)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && (pgErr.Code == "42P01" || pgErr.Code == "42703") {
			if !n.warn {
				n.warn = true
				slog.Warn("paramnotice: param_drop_notices does not exist yet (migration 029 not applied); first-drop audit events are skipped until it does")
			}
			return
		}
		slog.Error("paramnotice: failed to write first-drop audit event", "err", err)
		return
	}
	n.warn = false
	if len(n.seen) >= seenCap {
		n.seen = map[string]struct{}{}
	}
	n.seen[it.kind+"|"+it.id+"|"+it.param] = struct{}{}
}
