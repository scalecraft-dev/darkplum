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

	ModelAliasCreated EventType = "model_alias.created"
	ModelAliasUpdated EventType = "model_alias.updated"
	ModelAliasDeleted EventType = "model_alias.deleted"
	// RoutingPolicyChanged is no longer emitted (superseded by
	// RoutingPolicyOrgChanged) but the constant stays defined so the web's
	// event-type-to-label humaniser keeps rendering historical rows recorded
	// with it before this cutover.
	RoutingPolicyChanged EventType = "routing_policy.changed"
	// RoutingPolicyOrgChanged is recorded on PUT /api/v1/routing-policy
	// (the org-level policy). Detail carries the new residency/
	// allowed_models/default_model and, when the change tightens the
	// policy, affected_principals -- see routing.go's orgPolicyTightened.
	RoutingPolicyOrgChanged EventType = "routing_policy.org_changed"
	// RoutingPolicyPrincipalChanged is recorded on PUT/DELETE
	// .../routing-policy for a user or api_key principal. Detail carries
	// principal_type plus the before/after policy objects (policy shape
	// only, never content).
	RoutingPolicyPrincipalChanged EventType = "routing_policy.principal_changed"
	// SettingsSystemPromptChanged is recorded on PUT
	// /api/v1/settings/system-prompt. Detail carries whether a custom prompt
	// is now set and its length -- never the prompt text itself.
	SettingsSystemPromptChanged EventType = "settings.system_prompt_changed"
	// The admin-managed list of MCP server hosts tools may call.
	SettingsToolHostsChanged EventType = "settings.tool_hosts_changed"

	// Library (skills, context, tools). Metadata only: id, kind, slug,
	// visibility, sizes -- never an item's body.
	LibraryItemCreated           EventType = "library.created"
	LibraryItemUpdated           EventType = "library.updated"
	LibraryItemShared            EventType = "library.shared"
	LibraryItemUnshared          EventType = "library.unshared"
	LibraryItemDeleted           EventType = "library.deleted"
	LibraryImported              EventType = "library.imported"
	LibraryExported              EventType = "library.exported"
	LibraryOwnerReassigned       EventType = "library.owner_reassigned"
	LibraryToolVerified          EventType = "library.tool_verified"
	LibraryToolCredentialSet     EventType = "library.tool_credential_set"
	LibraryToolCredentialRemoved EventType = "library.tool_credential_removed"

	// ConversationDeleted is recorded on DELETE /api/v1/conversations/:id.
	// Detail carries only {"message_count": n} -- never conversation content
	// or title, which is exactly what a compliance reviewer needs (a
	// deletion happened, how big it was) without leaking chat content into
	// the admin-visible activity log. No other conversation endpoint writes
	// an activity event.
	ConversationDeleted EventType = "conversation.deleted"

	// ProviderKeyInstalled and ProviderKeyRotated are recorded on a
	// successful PUT /api/v1/providers/:vendor/key, distinguished by whether
	// the gateway reported the vendor as already `configured` before this
	// write (rotated) or not (installed) -- see providers.go's PutKey.
	// Detail carries {vendor, key_last4, model_filter_before,
	// model_filter_after}: key_last4 is computed from the SUBMITTED body
	// (never read back from storage, which never holds it anyway -- the key
	// itself is never stored in Postgres, only in the cluster Secret).
	// Anthropic only: workspace_id_before / workspace_id_after (not secret),
	// omitted when both are empty -- same on provider.filter_changed.
	ProviderKeyInstalled EventType = "provider.key_installed"
	ProviderKeyRotated   EventType = "provider.key_rotated"
	// ProviderKeyRemoved is recorded on a successful DELETE
	// /api/v1/providers/:vendor/key. Detail carries {vendor,
	// model_filter_before, model_filter_after} -- no key_last4 (there is no
	// submitted key on a removal).
	ProviderKeyRemoved EventType = "provider.key_removed"
	// ProviderFilterChanged is recorded on a successful PUT
	// /api/v1/providers/:vendor/filter. Detail carries {vendor,
	// model_filter_before, model_filter_after} -- no key_last4.
	ProviderFilterChanged EventType = "provider.filter_changed"

	// ProviderBillingKeyInstalled / Rotated / Removed are recorded on PUT /
	// DELETE /api/v1/providers/:vendor/billing-key (the separate admin-scope
	// key used only to read the vendor's usage and cost reports). Detail
	// carries {vendor, key_last4} (no key_last4 on removal); the key itself
	// is never stored anywhere but the cluster Secret.
	ProviderBillingKeyInstalled EventType = "provider.billing_key_installed"
	ProviderBillingKeyRotated   EventType = "provider.billing_key_rotated"
	ProviderBillingKeyRemoved   EventType = "provider.billing_key_removed"

	// ModelPriceChanged is recorded on every PUT/DELETE /api/v1/model-prices.
	// Detail carries {action: created|replaced|retired, before, after}
	// (price-row objects; null where not applicable).
	ModelPriceChanged EventType = "model_price.changed"
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
