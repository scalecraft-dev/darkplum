package provider

import (
	"context"
	"regexp"
)

var workspaceIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ValidWorkspaceID reports whether id is a well-formed Anthropic workspace id
// (^[A-Za-z0-9_-]{1,64}$). It is sent as an HTTP header value, so anything
// outside this alphabet is refused before it can reach a request.
func ValidWorkspaceID(id string) bool { return workspaceIDPattern.MatchString(id) }

type workspaceOverrideKey struct{}

// WithWorkspaceOverride returns a context that makes the anthropic provider
// use id (possibly "" = none) instead of the effective workspace id for the
// calls made under it. Used only by POST /v1/providers/anthropic/verify's
// installed-key path, so an admin can check a workspace id they are about to
// save with the key that is already installed.
func WithWorkspaceOverride(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, workspaceOverrideKey{}, id)
}

// WorkspaceOverrideFromContext returns the override set by WithWorkspaceOverride.
func WorkspaceOverrideFromContext(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(workspaceOverrideKey{}).(string)
	return id, ok
}
