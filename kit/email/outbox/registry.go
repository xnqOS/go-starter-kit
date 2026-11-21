package outbox

import (
	"sync"

	"github.com/xnqOS/go-starter-kit/kit/email"
)

// Registry holds every configured provider. The default sends rows queued
// without a provider name; a row records the provider it was queued for, so
// changing the default (or moving a sending domain to another provider) does
// not move mail that is already waiting.
type Registry struct {
	mu          sync.RWMutex
	defaultName string
	byName      map[string]email.Provider
}

// NewRegistry starts a registry whose default is p (nil: no default).
func NewRegistry(p email.Provider) *Registry {
	r := &Registry{byName: map[string]email.Provider{}}
	if p != nil {
		r.defaultName = p.Name()
		r.byName[p.Name()] = p
	}
	return r
}

// Add registers another provider; the default and an existing provider of
// the same name are unchanged.
func (r *Registry) Add(p email.Provider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, taken := r.byName[p.Name()]; !taken {
		r.byName[p.Name()] = p
	}
}

// Default is the name of the default provider.
func (r *Registry) Default() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.defaultName
}

// Get returns a provider by name; "" means the default.
func (r *Registry) Get(name string) (email.Provider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if name == "" {
		name = r.defaultName
	}
	p, ok := r.byName[name]
	return p, ok
}
