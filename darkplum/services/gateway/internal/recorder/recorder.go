package recorder

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// InferenceEvent is the data captured for each proxied request.
type InferenceEvent struct {
	RequestID        string
	APIKeyID         string
	UserID           string
	Model            string
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

// writeBatch inserts all events in a single pgx Batch round-trip.
func (r *Recorder) writeBatch(events []InferenceEvent) {
	b := &pgx.Batch{}
	for _, evt := range events {
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

	br := r.pool.SendBatch(context.Background(), b)
	defer br.Close()

	for range events {
		if _, err := br.Exec(); err != nil {
			slog.Error("failed to write inference event", "err", err)
		}
	}
}

// nullableStr returns nil when s is empty, so that Postgres UUID and INET
// columns receive NULL rather than an empty-string cast error.
func nullableStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
