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
	Stream           bool
	IP               string
	// Provider identifies the backend that served the request ("vllm", "lmstudio", or "bedrock").
	Provider string
	// Region is the cloud region for the backend. Populated for Bedrock, empty for vLLM.
	Region string
}

// Recorder buffers InferenceEvents and writes them to Postgres in batches.
// Record() is non-blocking: if the internal channel is full the event is
// dropped and a warning is logged rather than blocking the request path.
type Recorder struct {
	pool   *pgxpool.Pool
	events chan InferenceEvent

	// requestedModelSupported and nextSchemaProbe implement the
	// inference_events.requested_model schema-compatibility fallback. They
	// are only ever touched from the single flush() goroutine, so no
	// additional synchronisation is needed.
	requestedModelSupported bool
	nextSchemaProbe         time.Time
}

// New creates a Recorder and starts the background flush goroutine.
// Call Drain() during shutdown to flush remaining events.
func New(pool *pgxpool.Pool) *Recorder {
	r := &Recorder{
		pool:                    pool,
		events:                  make(chan InferenceEvent, 4096),
		requestedModelSupported: true, // assume present; corrected on first undefined-column error
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

// writeBatch inserts events, using the inference_events.requested_model
// column when it's believed to exist and falling back to the legacy column
// set (without requested_model) otherwise.
//
// requested_model and denial_code are added by the api's migrations 010 and
// 011 (the "routing columns"), but the gateway and
// api are deployed independently — a gateway pod can start before the api has
// run that migration against a freshly onboarded customer database. Without
// this fallback, every INSERT in the batch would fail with an
// "undefined_column" error and be dropped (the pre-existing, and in this one
// case unacceptable, behaviour of logging+dropping on any insert error): all
// inference events would be silently lost until the pod restarts. Instead,
// the first such failure downgrades to the legacy INSERT for this batch and
// future batches, and the code re-probes the new schema periodically so it
// starts using requested_model again once the api's migration lands, without
// requiring a gateway restart.
func (r *Recorder) writeBatch(events []InferenceEvent) {
	tryRequestedModel := r.requestedModelSupported
	if !tryRequestedModel && !time.Now().Before(r.nextSchemaProbe) {
		tryRequestedModel = true
	}

	if tryRequestedModel {
		missingColumn := r.execBatch(events, true)
		if !missingColumn {
			r.requestedModelSupported = true
			return
		}
		if r.requestedModelSupported {
			slog.Warn("inference_events.requested_model column not found; falling back to the legacy insert until the api's migration runs, re-probing periodically")
		}
		r.requestedModelSupported = false
		r.nextSchemaProbe = time.Now().Add(30 * time.Second)
	}

	r.execBatch(events, false)
}

// execBatch runs the insert for events using either the new (requested_model
// included) or legacy column set. It returns true if any event failed
// because requested_model is an undefined column, signalling the caller
// should retry the whole batch against the legacy schema. Any other
// per-event error is logged and that event is dropped — this matches the
// pre-existing (and still reasonable) behaviour for ordinary insert failures
// such as a bad status_code type or a transient connection error.
func (r *Recorder) execBatch(events []InferenceEvent, includeRequestedModel bool) bool {
	b := &pgx.Batch{}
	for _, evt := range events {
		if includeRequestedModel {
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
		} else {
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
	}

	br := r.pool.SendBatch(context.Background(), b)
	defer br.Close()

	undefinedColumn := false
	for range events {
		if _, err := br.Exec(); err != nil {
			if includeRequestedModel && isUndefinedColumnError(err) {
				undefinedColumn = true
				continue
			}
			slog.Error("failed to write inference event", "err", err)
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
