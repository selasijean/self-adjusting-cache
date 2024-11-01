package cache

import "context"

type Value[K comparable, V any] interface {
	Entry[K, V]
	// OnUpdate registers a callback that is called when the value is updated.
	OnUpdate(fn func(context.Context))
	// OnPurged registers a callback that is called when the value is purged from the cache.
	OnPurged(fn func(context.Context))
	// Direct dependents provides a list of keys in the cache that directly depend on the value.
	DirectDependents() []K
}

// Entry is a generic interface for an entry in the cache.
type Entry[K comparable, V any] interface {
	// Dependencies returns the dependencies of the cache entry.
	Dependencies() []K
	// Key returns the key of the cache entry
	Key() K
	// Value returns the value of the entry
	Value() V
}
