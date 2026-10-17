package recorder

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// undefinedColumnErrorCode is the Postgres SQLSTATE for "undefined_column".
// See https://www.postgresql.org/docs/current/errcodes-appendix.html.
const undefinedColumnErrorCode = "42703"

// InferenceEvent is the data captured for each proxied request.
type InferenceEvent struct {
	RequestID string
	APIKeyID  string
	UserID    string
	Model     string
	// RequestedModel is what the caller sent when it differs from Model
	// (i.e. an alias was resolved). Empty when no alias was used, which
	// nullableStr turns into a NULL requested_model column.
	RequestedModel string
	// DenialCode is the policy reason a request was refused before reaching a
	// provider (model_not_found, model_not_enabled, model_residency_violation,
	// policy_unavailable). Empty for requests that were dispatched.
	DenialCode       string
	StatusCode       int
	LatencyMS        int
	PromptTokens     *int
	CompletionTokens *int
	// CacheReadTokens / CacheCreationTokens are inference_events.
	// cache_read_tokens / cache_creation_tokens (migration 021): the cached
	// split of PromptTokens. nil (NULL) when the backend reported no usage;
	// 0 when usage was reported without cache fields.
	CacheReadTokens     *int
	CacheCreationTokens *int
	Stream              bool
	IP                  string
	// Provider identifies the backend that served the request ("vllm", "lmstudio", or "bedrock").
	Provider string
	// Region is the cloud region for the backend. Populated for Bedrock, empty for vLLM.
	Region string

	// Purpose is inference_events.purpose (migration 013): "inference" is
	// the only value the gateway writes today (see
	// queueRoutingSchema/purposeOrDefault) — "routing" and "routing_dryrun"
	// were only ever produced by the removed in-house model classifier;
	// historic rows may still carry them, but nothing writes them anymore.
	// Left empty by ordinary callers; purposeOrDefault fills in "inference"
	// at insert time.
	Purpose string
	// RouteName is the route a routed alias resolved to (empty for a static
	// alias, a plain model id, or when Purpose != "inference").
	RouteName string
	// RouterReason is the router package's one-line, closed-vocabulary
	// explanation for RouteName (see internal/router's Reason* constants).
	RouterReason string
	// RouterLatencyMS is how long the routing decision itself took. nil when
	// no routing decision was made (a static alias or plain model id).
	RouterLatencyMS *int

	// BackendClass is routing.Route.Class ("self_hosted", "cloud_managed",
	// "external_api") for the route that was resolved, written to the Gin
	// context the moment it is known (see handler.HandleChatCompletions) —
	// so a DENIED row (model_not_enabled, model_residency_violation,
	// model_not_allowed) carries it too, not only a dispatched one. Empty
	// for a denial reached before route resolution (model_not_found,
	// policy_unavailable, invalid_request, unsupported_parameter,
	// request_canceled), which nullableStr turns into NULL.
	BackendClass string
}

// Recorder buffers InferenceEvents and writes them to Postgres in batches.
// Record() is non-blocking: if the internal channel is full the event is
// dropped and a warning is logged rather than blocking the request path.
type Recorder struct {
	pool   *pgxpool.Pool
	events chan InferenceEvent

	// schemaLevel indexes into the package-level schemas slice: the column
	// set writeBatch currently believes inference_events supports. 0 (the
	// zero value) is schemas[0], the newest/most-current set — New() leaves
	// this unset deliberately, "assume present; corrected on first
	// undefined-column error", exactly as the pre-migration-013 code did for
	// requested_model.
	schemaLevel int
	// nextSchemaProbe implements the schema-compatibility re-probe: once
	// writeBatch has downgraded away from schemas[0], it retries from
	// schemas[0] again no more often than every 30s, so the recorder picks
	// a newly-landed migration back up without a pod restart.
	nextSchemaProbe time.Time
}

// New creates a Recorder and starts the background flush goroutine.
// Call Drain() during shutdown to flush remaining events.
func New(pool *pgxpool.Pool) *Recorder {
	r := &Recorder{
		pool:   pool,
		events: make(chan InferenceEvent, 4096),
	}
	go r.flush()
	return r
}

// Record enqueues an InferenceEvent. It never blocks.
func (r *Recorder) Record(evt InferenceEvent) {
	select {
	case r.events <- evt:
	default:
		slog.Warn("inference event buffer full, dropping event", "request_id", evt.RequestID)
	}
}

// Drain flushes all remaining events in the channel and returns.
// Call this during graceful shutdown after the HTTP server stops accepting requests.
func (r *Recorder) Drain() {
	close(r.events)
}

// flush drains the channel and writes batches every second or when 100 events
// have accumulated. It exits when the channel is closed (via Drain).
func (r *Recorder) flush() {
	batch := make([]InferenceEvent, 0, 100)
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case evt, ok := <-r.events:
			if !ok {
				if len(batch) > 0 {
					r.writeBatch(batch)
				}
				return
			}
			batch = append(batch, evt)
			if len(batch) >= 100 {
				r.writeBatch(batch)
				batch = batch[:0]
			}
		case <-ticker.C:
			if len(batch) > 0 {
				r.writeBatch(batch)
				batch = batch[:0]
			}
		}
	}
}

// schema describes one inference_events column set writeBatch can INSERT
// against, from the newest migration back to the very first shape. Keeping
// these as an ordered list — rather than nested "does column X exist" ifs —
// is what lets writeBatch step down (or back up) through however many
// migrations the gateway is currently ahead of or behind the api's, one
// undefined-column error at a time, without the branching growing with every
// future column this table gains.
type schema struct {
	// name identifies the schema in logs.
	name string
	// queue appends the INSERT for one event, in this schema's column set,
	// to b.
	queue func(b *pgx.Batch, evt InferenceEvent)
}

// schemas is ordered newest-first. writeBatch tries schemas[r.schemaLevel]
// first and walks forward through the slice on an undefined-column error
// until one succeeds (or the list is exhausted).
var schemas = []schema{
	// cache_tokens: migration 021 (cache_read_tokens, cache_creation_tokens)
	// plus everything below.
	{name: "cache_tokens", queue: queueCacheTokensSchema},
	// backend_class: migration 016 (inference_events.backend_class) plus
	// everything below. A gateway new enough to know about external_api
	// routes must still record correctly against a DB that hasn't run 016
	// yet — this rung is what lets it step down to "routing" on a 42703
	// exactly like every earlier column addition did.
	{name: "backend_class", queue: queueBackendClassSchema},
	// routing: migration 013 (purpose, route_name, router_reason,
	// router_latency_ms) plus everything below.
	{name: "routing", queue: queueRoutingSchema},
	// requested_model: migrations 010+011 (requested_model, denial_code) —
	// the shape the gateway spoke before routed aliases existed.
	{name: "requested_model", queue: queueRequestedModelSchema},
	// legacy: the original pre-010 shape.
	{name: "legacy", queue: queueLegacySchema},
}

// purposeOrDefault fills in the NOT NULL DEFAULT 'inference' the
// inference_events.purpose column has in Postgres: Go's zero value for
// InferenceEvent.Purpose is "", which would violate the column's CHECK
// constraint if sent as-is.
func purposeOrDefault(p string) string {
	if p == "" {
		return "inference"
	}
	return p
}

func queueCacheTokensSchema(b *pgx.Batch, evt InferenceEvent) {
	b.Queue(`
		INSERT INTO inference_events
			(request_id, api_key_id, user_id, model, requested_model, status_code, latency_ms,
			 prompt_tokens, completion_tokens, stream, ip_address,
			 provider, region, denial_code, purpose, route_name, router_reason, router_latency_ms,
			 backend_class, cache_read_tokens, cache_creation_tokens)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11::inet, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21)
	`,
		evt.RequestID,
		nullableStr(evt.APIKeyID),
		nullableStr(evt.UserID),
		evt.Model,
		nullableStr(evt.RequestedModel),
		evt.StatusCode,
		evt.LatencyMS,
		evt.PromptTokens,
		evt.CompletionTokens,
		evt.Stream,
		nullableStr(evt.IP),
		evt.Provider,
		nullableStr(evt.Region),
		nullableStr(evt.DenialCode),
		purposeOrDefault(evt.Purpose),
		nullableStr(evt.RouteName),
		nullableStr(evt.RouterReason),
		evt.RouterLatencyMS,
		nullableStr(evt.BackendClass),
		evt.CacheReadTokens,
		evt.CacheCreationTokens,
	)
}

func queueBackendClassSchema(b *pgx.Batch, evt InferenceEvent) {
	b.Queue(`
		INSERT INTO inference_events
			(request_id, api_key_id, user_id, model, requested_model, status_code, latency_ms,
			 prompt_tokens, completion_tokens, stream, ip_address,
			 provider, region, denial_code, purpose, route_name, router_reason, router_latency_ms,
			 backend_class)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11::inet, $12, $13, $14, $15, $16, $17, $18, $19)
	`,
		evt.RequestID,
		nullableStr(evt.APIKeyID),
		nullableStr(evt.UserID),
		evt.Model,
		nullableStr(evt.RequestedModel),
		evt.StatusCode,
		evt.LatencyMS,
		evt.PromptTokens,
		evt.CompletionTokens,
		evt.Stream,
		nullableStr(evt.IP),
		evt.Provider,
		nullableStr(evt.Region),
		nullableStr(evt.DenialCode),
		purposeOrDefault(evt.Purpose),
		nullableStr(evt.RouteName),
		nullableStr(evt.RouterReason),
		evt.RouterLatencyMS,
		nullableStr(evt.BackendClass),
	)
}

func queueRoutingSchema(b *pgx.Batch, evt InferenceEvent) {
	b.Queue(`
		INSERT INTO inference_events
			(request_id, api_key_id, user_id, model, requested_model, status_code, latency_ms,
			 prompt_tokens, completion_tokens, stream, ip_address,
			 provider, region, denial_code, purpose, route_name, router_reason, router_latency_ms)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11::inet, $12, $13, $14, $15, $16, $17, $18)
	`,
		evt.RequestID,
		nullableStr(evt.APIKeyID),
		nullableStr(evt.UserID),
		evt.Model,
		nullableStr(evt.RequestedModel),
		evt.StatusCode,
		evt.LatencyMS,
		evt.PromptTokens,
		evt.CompletionTokens,
		evt.Stream,
		nullableStr(evt.IP),
		evt.Provider,
		nullableStr(evt.Region),
		nullableStr(evt.DenialCode),
		purposeOrDefault(evt.Purpose),
		nullableStr(evt.RouteName),
		nullableStr(evt.RouterReason),
		evt.RouterLatencyMS,
	)
}

func queueRequestedModelSchema(b *pgx.Batch, evt InferenceEvent) {
	b.Queue(`
		INSERT INTO inference_events
			(request_id, api_key_id, user_id, model, requested_model, status_code, latency_ms,
			 prompt_tokens, completion_tokens, stream, ip_address,
			 provider, region, denial_code)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11::inet, $12, $13, $14)
	`,
		evt.RequestID,
		nullableStr(evt.APIKeyID),
		nullableStr(evt.UserID),
		evt.Model,
		nullableStr(evt.RequestedModel),
		evt.StatusCode,
		evt.LatencyMS,
		evt.PromptTokens,
		evt.CompletionTokens,
		evt.Stream,
		nullableStr(evt.IP),
		evt.Provider,
		nullableStr(evt.Region),
		nullableStr(evt.DenialCode),
	)
}

func queueLegacySchema(b *pgx.Batch, evt InferenceEvent) {
	b.Queue(`
		INSERT INTO inference_events
			(request_id, api_key_id, user_id, model, status_code, latency_ms,
			 prompt_tokens, completion_tokens, stream, ip_address,
			 provider, region)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::inet, $11, $12)
	`,
		evt.RequestID,
		nullableStr(evt.APIKeyID),
		nullableStr(evt.UserID),
		evt.Model,
		evt.StatusCode,
		evt.LatencyMS,
		evt.PromptTokens,
		evt.CompletionTokens,
		evt.Stream,
		nullableStr(evt.IP),
		evt.Provider,
		nullableStr(evt.Region),
	)
}

// writeBatch inserts events against the newest schema believed to be
// supported, walking forward through schemas on an undefined-column error
// until one succeeds. This is the gateway/api independent-deployment
// fallback: the gateway can start before the api has run whatever migration
// last touched inference_events (010/011 for requested_model/denial_code,
// 013 for purpose/route_name/router_reason/router_latency_ms, 016 for
// backend_class) against a freshly onboarded customer database. Without it,
// every INSERT in the batch would fail with "undefined_column" and be
// dropped — inference events silently lost until the pod restarts. Instead,
// a downgrade sticks for
// r.schemaLevel until the periodic re-probe (nextSchemaProbe) tries schemas[0]
// again, so the recorder picks a newly-landed migration back up without a
// restart.
func (r *Recorder) writeBatch(events []InferenceEvent) {
	level := r.schemaLevel
	if level > 0 && !time.Now().Before(r.nextSchemaProbe) {
		level = 0
	}

	for level < len(schemas) {
		missingColumn := r.execBatch(events, schemas[level])
		if !missingColumn {
			if level != r.schemaLevel {
				slog.Warn("inference_events schema level changed", "schema", schemas[level].name, "previous_schema", schemas[r.schemaLevel].name)
			}
			r.schemaLevel = level
			return
		}
		level++
		r.nextSchemaProbe = time.Now().Add(30 * time.Second)
	}

	// Every known schema 42703'd — even the legacy one, meaning the base
	// table itself is missing an original column. execBatch already logged
	// each row's error at the final (legacy) attempt; nothing more to try.
	r.schemaLevel = len(schemas) - 1
}

// execBatch runs the insert for events using s's column set. It returns true
// if any event failed because one of s's columns doesn't exist, signalling
// the caller should retry the whole batch against the next (older) schema.
// Any other per-event error is logged and that event is dropped — this
// matches the pre-existing (and still reasonable) behaviour for ordinary
// insert failures such as a bad status_code type or a transient connection
// error.
func (r *Recorder) execBatch(events []InferenceEvent, s schema) bool {
	b := &pgx.Batch{}
	for _, evt := range events {
		s.queue(b, evt)
	}

	br := r.pool.SendBatch(context.Background(), b)
	defer br.Close()

	undefinedColumn := false
	for range events {
		if _, err := br.Exec(); err != nil {
			if isUndefinedColumnError(err) {
				undefinedColumn = true
				continue
			}
			slog.Error("failed to write inference event", "err", err, "schema", s.name)
		}
	}
	return undefinedColumn
}

// isUndefinedColumnError reports whether err is a Postgres "undefined_column"
// error (SQLSTATE 42703) — the signature of a missing requested_model column.
func isUndefinedColumnError(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == undefinedColumnErrorCode
	}
	return false
}

// nullableStr returns nil when s is empty, so that Postgres UUID and INET
// columns receive NULL rather than an empty-string cast error.
func nullableStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
