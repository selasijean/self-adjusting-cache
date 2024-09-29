package cache

import (
	"context"
	"fmt"
	"runtime"
	"sync"

	"github.com/wcharczuk/go-incr"
)

type entry[K comparable, V any] struct {
	key   K
	value V
	deps  []Entry[K, V]
}

func (e *entry[K, V]) Key() K {
	return e.key
}

func (e *entry[K, V]) Value() V {
	return e.value
}

func (e *entry[K, V]) Dependencies() []Entry[K, V] {
	return e.deps
}

func NewEntry[K comparable, V any](key K, value V, deps []Entry[K, V]) Entry[K, V] {
	return &entry[K, V]{
		key:   key,
		value: value,
		deps:  deps,
	}
}

const (
	// DefaultMaxHeight is the default maximum number of keys that can
	// be tracked in the cache's dependency graph.
	DefaultMaxHeight = 20000
)

// OptCachePreallocateNodesSize preallocates the size of the cache
//
// If not provided, no size for elements will be preallocated.
func OptPreallocateSize(size int) func(*CacheOptions) {
	return func(c *CacheOptions) {
		c.PreallocateCacheSize = size
	}
}

// OptMaxHeightOfDependencyGraph caps the longest path within the cache's dependency graph
//
// If not provided, the default height is 20000.
func OptMaxHeightOfDependencyGraph(size int) func(*CacheOptions) {
	return func(c *CacheOptions) {
		c.MaxHeightOfDependencyGraph = size
	}
}

// OptUseParallelism enables parallel recomputation. numCPU sets the parallelism factor, or said another way
// the number of goroutines, to use
//
// numCPU will default to [runtime.NumCPU] if unset.
func OptUseParallelism(numCPU *int) func(*CacheOptions) {
	return func(c *CacheOptions) {
		c.EnableParallelism = true
		if numCPU == nil {
			c.Parallelism = runtime.NumCPU()
			return
		}

		if *numCPU < 1 {
			c.Parallelism = 1 // Ensure at least one goroutine
		} else {
			c.Parallelism = *numCPU
		}
	}
}

// CacheOptions are options for the cache.
type CacheOptions struct {
	MaxHeightOfDependencyGraph int
	PreallocateCacheSize       int
	Parallelism                int
	EnableParallelism          bool
}

type CacheOption func(*CacheOptions)

type cache[K comparable, V any] struct {
	graph             *incr.Graph
	nodes             map[K]*cacheNode[K, V]
	valueFn           func(ctx context.Context, key K) (Entry[K, V], error)
	writeBackFn       func(ctx context.Context, key K, value V) error
	cutoffFn          func(ctx context.Context, previous V, current V) (bool, error)
	enableParallelism bool

	mu              sync.RWMutex
	stabilizationMu sync.Mutex
}

func New[K comparable, V any](valueFn func(ctx context.Context, key K) (Entry[K, V], error), opts ...CacheOption) Cache[K, V] {
	if valueFn == nil {
		panic("valueFn is not set")
	}

	options := CacheOptions{
		MaxHeightOfDependencyGraph: DefaultMaxHeight,
	}
	for _, opt := range opts {
		opt(&options)
	}

	return &cache[K, V]{
		nodes: make(map[K]*cacheNode[K, V], options.PreallocateCacheSize),
		graph: incr.New(
			incr.OptGraphClearRecomputeHeapOnError(true),
			incr.OptGraphMaxHeight(options.MaxHeightOfDependencyGraph),
			incr.OptGraphPreallocateNodesSize(options.PreallocateCacheSize),
			incr.OptGraphParallelism(options.Parallelism),
		),
		valueFn:           valueFn,
		enableParallelism: options.EnableParallelism,
	}
}

func (c *cache[K, V]) Put(ctx context.Context, entries ...Entry[K, V]) error {
	err := withWriteLock(&c.mu, func() error {
		visited := make(map[K]bool)
		for _, entry := range entries {
			node, err := c.unsafePut(ctx, entry.Key(), entry.Value(), entry.Dependencies(), visited)
			if err != nil {
				return err
			}

			err = node.observe()
			if err != nil {
				return err
			}
		}

		return nil
	})
	if err != nil {
		return err
	}

	c.stabilizationMu.Lock()
	defer c.stabilizationMu.Unlock()

	if c.enableParallelism {
		return c.graph.ParallelStabilize(ctx)
	}

	return c.graph.Stabilize(ctx)
}

func (c *cache[K, V]) Get(key K) (Value[K, V], bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	node, ok := c.nodes[key]
	if !ok || !node.IsValid() {
		return nil, false
	}

	return node, ok
}

func (c *cache[K, V]) Recompute(ctx context.Context, keys ...K) error {
	results := make([]Entry[K, V], 0, len(keys))

	err := withReadLock(&c.mu, func() error {
		nodes := make([]*cacheNode[K, V], 0, len(keys))
		for _, key := range keys {
			node, ok := c.nodes[key]
			if !ok {
				return fmt.Errorf("key not found in cache: %v", key)
			}

			nodes = append(nodes, node)
		}

		sortByHeight(nodes)
		for _, node := range nodes {
			node.MarkAsInvalid()
			result, err := c.valueFn(ctx, node.Key())
			if err != nil {
				return err
			}
			node.MarkAsValid()
			results = append(results, result)
		}

		return nil
	})
	if err != nil {
		return err
	}

	return c.Put(ctx, results...)
}

func (c *cache[K, V]) WithWriteBackFn(fn func(ctx context.Context, key K, value V) error) Cache[K, V] {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.writeBackFn = fn
	return c
}

func (c *cache[K, V]) WithParallelism(enabled bool) Cache[K, V] {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.enableParallelism = enabled
	return c
}

func (c *cache[K, V]) WithCutoffFn(fn func(ctx context.Context, previous V, current V) (bool, error)) Cache[K, V] {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.cutoffFn = fn
	return c
}

func (c *cache[K, V]) unsafePut(ctx context.Context, key K, value V, dependencies []Entry[K, V], visited map[K]bool) (*cacheNode[K, V], error) {
	if visited[key] {
		return nil, fmt.Errorf("cyclic dependency detected for key: %v", key)
	}

	visited[key] = true
	defer func() {
		delete(visited, key)
	}()

	for _, dependency := range dependencies {
		depKey := dependency.Key()
		depValue := dependency.Value()
		depDeps := dependency.Dependencies()

		_, err := c.unsafePut(ctx, depKey, depValue, depDeps, visited)
		if err != nil {
			return nil, err
		}
	}

	node, ok := c.nodes[key]
	if ok {
		node.withDependencies(dependencies).withInitialValue(value).
			reconstructDependencyGraph()
	} else {
		node = newCacheNode(c, key, value, dependencies)
		c.nodes[key] = node
	}

	return node, nil
}
