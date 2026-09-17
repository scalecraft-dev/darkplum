package middleware

import "testing"

// TestResolveAttributedUserID_ServiceKeyHonoursHeader mirrors the api
// service's proxy path: it authenticates with one shared service key on
// behalf of many session users, so the session user id must come from the
// header it sets.
func TestResolveAttributedUserID_ServiceKeyHonoursHeader(t *testing.T) {
	got := resolveAttributedUserID(KeyTypeService, "", "session-user-42")
	if got != "session-user-42" {
		t.Errorf("resolveAttributedUserID = %q, want header value for a service key", got)
	}
}

// TestResolveAttributedUserID_UserKeyIgnoresHeader is the audit-spoofing
// regression guard: a caller authenticated with their OWN user API key must
// never be able to attribute their traffic to a different user_id by setting
// X-Darkplum-User-Id on the request themselves.
func TestResolveAttributedUserID_UserKeyIgnoresHeader(t *testing.T) {
	got := resolveAttributedUserID(KeyTypeUser, "victim-user-1", "attacker-controlled-header-value")
	if got != "victim-user-1" {
		t.Errorf("resolveAttributedUserID = %q, want the key's own user_id (victim-user-1), header must be ignored for a user key", got)
	}
}

func TestResolveAttributedUserID_ServiceKeyNoHeaderFallsBackToKeyUserID(t *testing.T) {
	got := resolveAttributedUserID(KeyTypeService, "", "")
	if got != "" {
		t.Errorf("resolveAttributedUserID = %q, want empty when a service key request carries no attribution header", got)
	}
}

func TestResolveAttributedUserID_UnknownKeyTypeIgnoresHeader(t *testing.T) {
	// Defensive: an empty/unrecognised key_type (e.g. context key never set)
	// must not be treated as trusted, only the explicit "service" type is.
	got := resolveAttributedUserID("", "user-1", "someone-else")
	if got != "user-1" {
		t.Errorf("resolveAttributedUserID = %q, want the key's own user_id for a non-service key_type", got)
	}
}
