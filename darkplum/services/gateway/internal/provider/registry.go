package provider

import "sync"

// Registry is a thread-safe map from provider key (routing.Route.ProviderKey)
// to the Provider instance that serves it. Entries are added once at startup
// for statically-configured backends (vllm, lmstudio, mock) and lazily for
// backends whose underlying client is constructed on first successful
// discovery — Bedrock, via routing.NewBedrockSource's onReady callback, to
// support retrying provider init on later discovery ticks instead of leaving
// it "unavailable until restart".
type Registry struct {
	mu    sync.RWMutex
	byKey map[string]Provider
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{byKey: make(map[string]Provider)}
}

// Set registers or replaces the provider for key. Safe for concurrent use.
func (r *Registry) Set(key string, p Provider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byKey[key] = p
}

// Get returns the provider registered for key, if any. Safe for concurrent use.
func (r *Registry) Get(key string) (Provider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.byKey[key]
	return p, ok
}
