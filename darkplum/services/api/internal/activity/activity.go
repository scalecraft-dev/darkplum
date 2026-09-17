package activity

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
)

// EventType is the string tag stored in activity_events.event_type.
type EventType string

const (
	UserLogin           EventType = "user.login"
	UserLoginFailed     EventType = "user.login_failed"
	UserLogout          EventType = "user.logout"
	UserCreated         EventType = "user.created"
	UserDeleted         EventType = "user.deleted"
	UserRoleChanged     EventType = "user.role_changed"
	UserPasswordReset   EventType = "user.password_reset"
	UserPasswordChanged EventType = "user.password_changed"
	APIKeyCreated       EventType = "apikey.created"
	APIKeyRevoked       EventType = "apikey.revoked"
)

// Event is the input to Recorder.Record.
type Event struct {
	Type       EventType
	ActorID    *string
	TargetType *string
	TargetID   *string
	Detail     map[string]any
	IP         string
	RequestID  string
}

// Recorder writes activity events to Postgres.
// All writes are fire-and-forget: errors are logged but never returned to the caller.
type Recorder struct {
	pool *pgxpool.Pool
}

// NewRecorder returns a Recorder backed by pool.
func NewRecorder(pool *pgxpool.Pool) *Recorder {
	return &Recorder{pool: pool}
}

// Record inserts an activity event. It uses context.Background() so that a
// cancelled request context (e.g. client disconnect) does not suppress the write.
func (r *Recorder) Record(_ context.Context, evt Event) {
	var detailJSON []byte
	if evt.Detail != nil {
		var err error
		detailJSON, err = json.Marshal(evt.Detail)
		if err != nil {
			slog.Error("activity: marshal detail", "event_type", evt.Type, "err", err)
			return
		}
	}

	_, err := r.pool.Exec(context.Background(), `
		INSERT INTO activity_events
			(event_type, actor_id, target_type, target_id, detail, ip_address, request_id)
		VALUES ($1, $2, $3, $4, $5, $6::inet, $7)
	`, string(evt.Type), evt.ActorID, evt.TargetType, evt.TargetID,
		detailJSON, nullableIP(evt.IP), evt.RequestID)
	if err != nil {
		slog.Error("activity: failed to record event",
			"event_type", evt.Type, "err", err)
	}
}

// FromContext builds a base Event from Gin context values, leaving TargetType,
// TargetID, and Detail for the caller to set.
func FromContext(c *gin.Context, eventType EventType) Event {
	var actorID *string
	if id, ok := c.Get("user_id"); ok {
		if s, ok := id.(string); ok && s != "" {
			actorID = &s
		}
	}

	var reqID string
	if rid, ok := c.Get("request_id"); ok {
		if s, ok := rid.(string); ok {
			reqID = s
		}
	}

	return Event{
		Type:      eventType,
		ActorID:   actorID,
		IP:        c.ClientIP(),
		RequestID: reqID,
	}
}

// strPtr is a convenience helper to take the address of a string literal.
func StrPtr(s string) *string { return &s }

// nullableIP returns nil when ip is empty so that the INET cast on an empty
// string does not produce a Postgres error.
func nullableIP(ip string) *string {
	if ip == "" {
		return nil
	}
	return &ip
}
