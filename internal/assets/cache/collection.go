package cache

import (
	"sync"

	"github.com/simulot/immich-go/internal/gen/syncmap"
	"github.com/simulot/immich-go/internal/gen/syncset"
)

type saveFn[T any] func(coll T, ids []string) (T, error)

type CollectionCache[T comparable] struct {
	mu           sync.Mutex
	maxCacheSize int
	collections  *syncmap.SyncMap[string, *Collection[T]] // collection of Collections
	saveFn       saveFn[T]
}

// NewCollectionCache creates a new collection cache with the given max cache size.
func NewCollectionCache[T comparable](maxCacheSize int, saveFn saveFn[T]) *CollectionCache[T] {
	return &CollectionCache[T]{
		maxCacheSize: maxCacheSize,
		collections:  syncmap.New[string, *Collection[T]](),
		saveFn:       saveFn,
	}
}

func (cc *CollectionCache[T]) getOrCreate(key string, coll T, initialIDs []string) (*Collection[T], bool) {
	if c, ok := cc.collections.Load(key); ok {
		return c, false
	}
	cc.mu.Lock()
	defer cc.mu.Unlock()
	if c, ok := cc.collections.Load(key); ok {
		return c, false
	}
	c := newCollection(coll, cc.maxCacheSize, cc.saveFn, initialIDs, nil)
	cc.collections.Store(key, c)
	return c, true
}

// NewCollection manage the cache for the given collection and the given list of initial ids.
// If the collection already exists, the ids are added to the existing collection.
func (cc *CollectionCache[T]) NewCollection(key string, coll T, ids []string) {
	c, created := cc.getOrCreate(key, coll, ids)
	if !created {
		for _, id := range ids {
			c.addID(id)
		}
	}
}

func (cc *CollectionCache[T]) AddIDToCollection(key string, collObj T, id string) bool {
	c, _ := cc.getOrCreate(key, collObj, nil)
	return c.addID(id)
}

func (cc *CollectionCache[T]) GetCollections() *syncmap.SyncMap[string, *Collection[T]] {
	return cc.collections
}

func (cc *CollectionCache[T]) GetCollection(key string) (T, []string, bool) {
	c, ok := cc.collections.Load(key)
	if !ok {
		var zero T
		return zero, nil, false
	}
	return c.collection, c.Items(), true
}

func (cc *CollectionCache[T]) Close() {
	cc.collections.Range(func(key string, c *Collection[T]) bool {
		c.close()
		return true
	})
}

type Collection[T comparable] struct {
	mu           sync.Mutex
	collection   T
	items        *syncset.Set[string]
	newItems     *syncset.Set[string]
	saveFn       saveFn[T]
	maxCacheSize int
}

func newCollection[T comparable](collection T, maxCacheSize int, saveFn saveFn[T], initialIDs []string, newIDs []string) *Collection[T] {
	c := &Collection[T]{
		collection:   collection,
		items:        syncset.New(initialIDs...),
		newItems:     syncset.New(newIDs...),
		saveFn:       saveFn,
		maxCacheSize: maxCacheSize,
	}

	return c
}

func (c *Collection[T]) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if items := c.newItems.Items(); len(items) > 0 {
		if updated, err := c.saveFn(c.collection, items); err == nil {
			c.collection = updated
		}
		c.newItems = syncset.New[string]()
	}
}

func (c *Collection[T]) Items() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.items.Items()
}

func (c *Collection[T]) NewItems() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.newItems.Items()
}

func (c *Collection[T]) addID(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	added := false
	if c.items.Add(id) {
		added = true
		c.newItems.Add(id)
		if c.newItems.Len() >= c.maxCacheSize {
			// err is logged in saveFn; update collection metadata (e.g. created Album/Tag ID)
			if updated, err := c.saveFn(c.collection, c.newItems.Items()); err == nil {
				c.collection = updated
			}

			// a fresh set of assets, even if the save failed, to avoid retrying the same assets endlessly
			c.newItems = syncset.New[string]()
		}
	}
	return added
}
