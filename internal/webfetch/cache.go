package webfetch

import (
	"container/list"
	"sync"
	"time"
)

// ttlCache is a small LRU cache with per-entry expiry. Search results
// repeatedly surface the same URLs (across queries in one turn and across
// turns), so caching the extracted text avoids refetching pages.
type ttlCache struct {
	mu    sync.Mutex
	max   int
	ttl   time.Duration
	ll    *list.List // front = most recently used
	items map[string]*list.Element
	now   func() time.Time // injectable for tests
}

type cacheEntry struct {
	key     string
	page    Page
	expires time.Time
}

func newTTLCache(maxEntries int, ttl time.Duration) *ttlCache {
	return &ttlCache{
		max:   maxEntries,
		ttl:   ttl,
		ll:    list.New(),
		items: make(map[string]*list.Element),
		now:   time.Now,
	}
}

func (c *ttlCache) get(key string) (Page, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return Page{}, false
	}
	entry := el.Value.(*cacheEntry)
	if c.now().After(entry.expires) {
		c.ll.Remove(el)
		delete(c.items, key)
		return Page{}, false
	}
	c.ll.MoveToFront(el)
	return entry.page, true
}

func (c *ttlCache) put(key string, page Page) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		entry := el.Value.(*cacheEntry)
		entry.page = page
		entry.expires = c.now().Add(c.ttl)
		c.ll.MoveToFront(el)
		return
	}
	el := c.ll.PushFront(&cacheEntry{key: key, page: page, expires: c.now().Add(c.ttl)})
	c.items[key] = el
	if c.ll.Len() > c.max {
		oldest := c.ll.Back()
		c.ll.Remove(oldest)
		delete(c.items, oldest.Value.(*cacheEntry).key)
	}
}
